//go:build enable_vips

package thumbnail

import "github.com/davidbyttow/govips/v2/vips"

func init() {
	// temporary remove TIFF and JP2K from go-vips' list of supported
	// imagetypes
	delete(vips.ImageTypes, vips.ImageTypeTIFF)
	delete(vips.ImageTypes, vips.ImageTypeJP2K)
}

var (
	// SupportedMimeTypes contains an all mimetypes which are supported by the thumbnailer.
	SupportedMimeTypes = map[string]struct{}{
		"image/png":                         {},
		"image/jpg":                         {},
		"image/jpeg":                        {},
		"image/gif":                         {},
		"image/bmp":                         {},
		"image/x-ms-bmp":                    {},
		"text/plain":                        {},
		"audio/flac":                        {},
		"audio/mpeg":                        {},
		"audio/ogg":                         {},
		"application/vnd.geogebra.slides":   {},
		"application/vnd.geogebra.pinboard": {},
		"image/webp":                        {},
	}
)
