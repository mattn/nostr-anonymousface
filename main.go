package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	pigo "github.com/esimov/pigo/core"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/nfnt/resize"
	"golang.org/x/image/draw"
)

var (
	maskImg    image.Image
	classifier *pigo.Pigo
	page       string

	//go:embed static
	assets embed.FS
)

func init() {
	sub, _ := fs.Sub(assets, "static")

	f, err := sub.Open("index.html")
	if err != nil {
		log.Fatal("cannot open mask.png:", err)
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal("cannot read facefinder:", err)
	}
	page = string(b)

	f, err = sub.Open("mask.png")
	if err != nil {
		log.Fatal("cannot open mask.png:", err)
	}
	defer f.Close()
	maskImg, _, err = image.Decode(f)
	if err != nil {
		log.Fatal("cannot decode mask.png:", err)
	}

	f, err = sub.Open("facefinder")
	if err != nil {
		log.Fatal("cannot open facefinder:", err)
	}
	defer f.Close()
	b, err = ioutil.ReadAll(f)
	if err != nil {
		log.Fatal("cannot read facefinder:", err)
	}

	pigo := pigo.NewPigo()
	classifier, err = pigo.Unpack(b)
	if err != nil {
		log.Fatal("cannot unpack facefinder:", err)
	}
}

func upload(buf *bytes.Buffer) (string, error) {
	req, err := http.NewRequest(http.MethodPost, "https://void.cat/upload?cli=true", buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("V-Content-Type", "image/png")
	result := sha256.Sum256(buf.Bytes())
	req.Header.Set("V-Full-Digest", hex.EncodeToString(result[:]))
	req.Header.Set("V-Filename", "image.png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer req.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func main() {
	nsec := os.Getenv("ANONYMOUSFACE_NSEC")
	if nsec == "" {
		log.Fatal("ANONYMOUSFACE_NSEC is not set")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "text/plain")

		if r.Method == "GET" {
			fmt.Fprintln(w, "curl -i -X POST -F image=@input.jpg http://anonymousface.compile-error.net > out.jpg")
			return
		}

		var ev nostr.Event
		err := json.NewDecoder(r.Body).Decode(&ev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ev.Tags.ContainsAny("t", []string{"anonymousface"}) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		urlPattern := `https?://[-A-Za-z0-9+&@#\/%?=~_|!:,.;\(\)*]+`
		urlRe := regexp.MustCompile(urlPattern)
		matches := urlRe.FindAllStringSubmatchIndex(ev.Content, -1)
		if len(matches) == 0 {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		u := ev.Content[matches[0][0]:matches[0][1]]
		resp, err := http.Get(u)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		img, _, err := image.Decode(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		bounds := img.Bounds().Max
		param := pigo.CascadeParams{
			MinSize:     20,
			MaxSize:     2000,
			ShiftFactor: 0.1,
			ScaleFactor: 1.1,
			ImageParams: pigo.ImageParams{
				Pixels: pigo.RgbToGrayscale(pigo.ImgToNRGBA(img)),
				Rows:   bounds.Y,
				Cols:   bounds.X,
				Dim:    bounds.X,
			},
		}
		faces := classifier.RunCascade(param, 0)
		faces = classifier.ClusterDetections(faces, 0.18)

		canvas := image.NewRGBA(img.Bounds())
		draw.Draw(canvas, img.Bounds(), img, image.Point{0, 0}, draw.Over)
		for _, face := range faces {
			pt := image.Point{face.Col - face.Scale/2, face.Row - face.Scale/2}
			fimg := resize.Resize(uint(face.Scale), uint(face.Scale), maskImg, resize.NearestNeighbor)
			draw.Copy(canvas, pt, fimg, fimg.Bounds(), draw.Over, nil)
		}

		var buf bytes.Buffer
		err = png.Encode(&buf, canvas)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		uploaded, err := upload(&buf)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.HasPrefix(uploaded, "http://") {
			uploaded = "https" + uploaded[4:]
		}
		uploaded += ".png"

		var sk string
		if _, s, err := nip19.Decode(nsec); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			sk = s.(string)
		}
		if pub, err := nostr.GetPublicKey(sk); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			ev.PubKey = pub
		}
		ev.Tags = []nostr.Tag{{"e", ev.PubKey, "", "reply"}}
		ev.CreatedAt = nostr.Now()
		ev.Content = uploaded
		ev.Sign(sk)

		w.Header().Add("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ev)
	})
	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8080"
	}
	http.ListenAndServe(addr, nil)
}
