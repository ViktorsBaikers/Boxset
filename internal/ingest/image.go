package ingest

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif"  // register the three artwork source formats
	_ "image/jpeg" // register the three artwork source formats
	_ "image/png"  // register the three artwork source formats
	"io"
)

// Image decodes one artwork source under the image limits: at most
// maxImageBytes encoded, every side from 1 to maxImageSide pixels and at most
// maxImagePixels of decoded area. The header decides the bounds before any pixel
// buffer exists; the decoded image is checked again as a belt-and-braces
// invariant, not as a bound reached by a known input. None of the three
// registered decoders can report a canvas larger than its header: PNG and JPEG
// decode at the dimensions their header declares, and the standard library gif
// decoder refuses a frame larger than the logical screen. Only a format
// registered elsewhere could break that, so the post-decode check has no test
// that drives it.
func Image(r io.Reader) (image.Image, string, error) {
	data, err := readLimited(r, maxImageBytes, reasonImageTooLarge)
	if err != nil {
		var refused *Error
		if errors.As(err, &refused) {
			return nil, "", err
		}
		return nil, "", &Error{Reason: reasonImageMalformed, Params: map[string]string{"stage": "read"}, cause: err}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", &Error{Reason: reasonImageMalformed, Params: map[string]string{"stage": "header"}, cause: err}
	}
	if refused := imageBounds(config.Width, config.Height); refused != nil {
		return nil, "", refused
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", &Error{Reason: reasonImageMalformed, Params: map[string]string{"stage": "decoded"}, cause: err}
	}
	bounds := decoded.Bounds()
	if refused := imageBounds(bounds.Dx(), bounds.Dy()); refused != nil {
		return nil, "", refused
	}
	return decoded, format, nil
}

// imageBounds requires both sides and the decoded area to be inside the limits.
// Callers apply it to the header dimensions and again to the decoded bounds; see
// Image for why the second application cannot refuse under the registered
// decoders.
func imageBounds(width, height int) *Error {
	if width < 1 || height < 1 || width > maxImageSide || height > maxImageSide {
		return &Error{Reason: reasonImageDimensionsInvalid, Params: map[string]string{
			"width":      count(int64(width)),
			"height":     count(int64(height)),
			"limit_side": count(maxImageSide),
		}}
	}
	if pixels := int64(width) * int64(height); pixels > maxImagePixels {
		return &Error{Reason: reasonImagePixelsExceeded, Params: map[string]string{
			"width":        count(int64(width)),
			"height":       count(int64(height)),
			"pixels":       count(pixels),
			"limit_pixels": count(maxImagePixels),
		}}
	}
	return nil
}
