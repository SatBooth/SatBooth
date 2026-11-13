// Package qr renders text as a QR code in SVG form, for the checkout page's Lightning invoice.
//
// The encoder is our own copy of the coding and gf256 packages of rsc.io/qr v0.2.0 (see
// LICENSE); this file only chooses the mode and the version, and draws the result. The
// error-correction level is always M. The mask is fixed (mask 0), as in rsc.io/qr.
package qr

import (
	"bytes"
	"errors"
	"strconv"

	"github.com/satbooth/satbooth/qr/coding"
)

// MaxInput is the longest input SVG accepts, in bytes.
const MaxInput = 1200

// quiet is the width of the quiet zone, in modules, as the QR standard asks.
const quiet = 4

// scale is the default drawing size of one module, in SVG user units; the viewBox is in modules,
// so the page can resize the image freely.
const scale = 8

// The errors are fixed values; neither carries any part of the input.
var (
	ErrEmpty   = errors.New("qr: empty input")
	ErrTooLong = errors.New("qr: input too long")
)

// encode picks alphanumeric mode when every byte is in the QR alphanumeric set and byte mode
// otherwise, then the smallest version that holds the text at level M.
func encode(text string) (*coding.Code, coding.Version, error) {
	if len(text) == 0 {
		return nil, 0, ErrEmpty
	}
	if len(text) > MaxInput {
		return nil, 0, ErrTooLong
	}
	var enc coding.Encoding = coding.String(text)
	if a := coding.Alpha(text); a.Check() == nil {
		enc = a
	}
	for v := coding.Version(coding.MinVersion); v <= coding.MaxVersion; v++ {
		if enc.Bits(v) > v.DataBytes(coding.M)*8 {
			continue
		}
		p, err := coding.NewPlan(v, coding.M, 0)
		if err != nil {
			return nil, 0, err
		}
		c, err := p.Encode(enc)
		if err != nil {
			return nil, 0, err
		}
		return c, v, nil
	}
	// Unreachable: 1,200 bytes fit version 40-M in both modes.
	return nil, 0, ErrTooLong
}

// SVG returns text as a standalone SVG QR code: a white square and one black path of M/h/v
// commands, a 4-module quiet zone, shape-rendering crispEdges and a viewBox in modules. The
// document has no text, script, style or external reference. The input is at most MaxInput
// bytes and not empty; otherwise the error is ErrEmpty or ErrTooLong.
func SVG(text string) ([]byte, error) {
	c, _, err := encode(text)
	if err != nil {
		return nil, err
	}
	n := c.Size + 2*quiet
	num := strconv.Itoa
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + num(n) + ` ` + num(n) +
		`" width="` + num(n*scale) + `" height="` + num(n*scale) + `" shape-rendering="crispEdges">`)
	b.WriteString(`<rect width="` + num(n) + `" height="` + num(n) + `" fill="#fff"/>`)
	b.WriteString(`<path fill="#000" d="`)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; {
			if !c.Black(x, y) {
				x++
				continue
			}
			start := x
			for x < c.Size && c.Black(x, y) {
				x++
			}
			w := num(x - start)
			b.WriteString("M" + num(start+quiet) + " " + num(y+quiet) + "h" + w + "v1h-" + w)
		}
	}
	b.WriteString(`"/></svg>`)
	b.WriteByte('\n')
	return b.Bytes(), nil
}
