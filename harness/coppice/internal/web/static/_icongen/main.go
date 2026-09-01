// Run once: go run ./internal/web/static/_icongen/main.go
// The leading underscore keeps this directory out of ./... and out of embed.
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// A coppice is a stump that sends up straight shoots. Three shoots of
// different heights on a stool, in flat colour. Legible at 48 pixels, and
// nothing here needs a font.
func icon(size int) *image.RGBA {
	ground := color.RGBA{0x12, 0x18, 0x1a, 0xff}
	shoot := color.RGBA{0x8f, 0xb9, 0x96, 0xff}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{ground}, image.Point{}, draw.Src)

	unit := float64(size) / 32
	rect := func(x, y, w, h float64) {
		r := image.Rect(int(x*unit), int(y*unit), int((x+w)*unit), int((y+h)*unit))
		draw.Draw(img, r, &image.Uniform{shoot}, image.Point{}, draw.Src)
	}
	// the stool
	rect(7, 24, 18, 3)
	// three shoots
	rect(9, 12, 3, 12)
	rect(14.5, 6, 3, 18)
	rect(20, 10, 3, 14)
	return img
}

func main() {
	dir := "internal/web/static/icons"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, size := range []int{192, 512} {
		name := filepath.Join(dir, "icon-"+strconv.Itoa(size)+".png")
		f, err := os.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, icon(size)); err != nil {
			log.Fatal(err)
		}
		f.Close()
		log.Printf("wrote %s", name)
	}
}
