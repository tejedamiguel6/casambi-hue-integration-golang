package audio

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"sort"
)

// DominantColors holds the primary and secondary colors extracted from album art.
type DominantColors struct {
	Primary   HSV
	Secondary HSV
	ImageURL  string
}

// HSV represents a color in Hue-Saturation-Value space.
// Hue: 0-65535 (Philips Hue scale), Sat: 0-254, Val: 0-254
type HSV struct {
	Hue int `json:"hue"` // 0-65535
	Sat int `json:"sat"` // 0-254
	Val int `json:"val"` // 0-254
}

// ExtractColorsFromURL downloads an image and extracts the two most dominant vibrant colors.
func ExtractColorsFromURL(imageURL string) (*DominantColors, error) {
	resp, err := http.Get(imageURL)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	colors := extractTopColors(img, 2)
	if len(colors) == 0 {
		return &DominantColors{
			Primary:   HSV{Hue: 0, Sat: 200, Val: 254},
			Secondary: HSV{Hue: 43690, Sat: 200, Val: 254},
			ImageURL:  imageURL,
		}, nil
	}

	result := &DominantColors{
		Primary:  colors[0],
		ImageURL: imageURL,
	}
	if len(colors) > 1 {
		result.Secondary = colors[1]
	} else {
		// Complementary color
		result.Secondary = HSV{
			Hue: (colors[0].Hue + 32768) % 65536,
			Sat: colors[0].Sat,
			Val: colors[0].Val,
		}
	}

	return result, nil
}

// extractTopColors finds the N most dominant vibrant colors in an image.
func extractTopColors(img image.Image, n int) []HSV {
	bounds := img.Bounds()

	// Sample pixels (skip some for speed)
	step := 4
	type colorBucket struct {
		hue, sat, val float64
		count         int
	}

	// Quantize hue into 12 buckets (30° each on 360° wheel)
	// Bucket 0 = red (covers 0-30° AND 330-360° to handle red wrap-around)
	buckets := make([]colorBucket, 12)

	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			// Convert from 16-bit to 8-bit
			rf := float64(r>>8) / 255.0
			gf := float64(g>>8) / 255.0
			bf := float64(b>>8) / 255.0

			h, s, v := rgbToHSV(rf, gf, bf)

			// Skip dark, desaturated, or near-white pixels — we want vivid colors
			if s < 0.25 || v < 0.15 {
				continue
			}

			// Merge 330-360° into bucket 0 (red wrap-around)
			bucket := int(h/30.0) % 12
			if bucket == 11 {
				bucket = 0
				h = h - 360 // normalize so average stays near 0°
			}
			buckets[bucket].hue += h
			buckets[bucket].sat += s
			buckets[bucket].val += v
			buckets[bucket].count++
		}
	}

	// Sort buckets by count (most pixels first), then by saturation
	type scored struct {
		idx   int
		score float64
	}
	var scores []scored
	for i, b := range buckets {
		if b.count > 0 {
			avgSat := b.sat / float64(b.count)
			// Score = pixel count weighted by saturation (prefer vibrant colors)
			scores = append(scores, scored{i, float64(b.count) * avgSat})
		}
	}
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	var result []HSV
	for i := 0; i < n && i < len(scores); i++ {
		b := buckets[scores[i].idx]
		avgHue := b.hue / float64(b.count)
		avgSat := b.sat / float64(b.count)

		// Handle negative hue from red wrap-around
		if avgHue < 0 {
			avgHue += 360
		}

		// Use full value and boost saturation — LEDs need high saturation to show color.
		// Album art colors are often muted, but we want vivid lighting.
		boostedSat := clampF(avgSat*1.5, 0.7, 1.0) // boost and floor at 70%
		result = append(result, HSV{
			Hue: int(avgHue / 360.0 * 65535),
			Sat: int(boostedSat * 254),
			Val: 254,
		})
	}

	return result
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func rgbToHSV(r, g, b float64) (h, s, v float64) {
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	v = max
	d := max - min

	if max == 0 {
		s = 0
	} else {
		s = d / max
	}

	if d == 0 {
		h = 0
	} else {
		switch max {
		case r:
			h = (g - b) / d
			if g < b {
				h += 6
			}
		case g:
			h = (b-r)/d + 2
		case b:
			h = (r-g)/d + 4
		}
		h *= 60
	}
	return
}
