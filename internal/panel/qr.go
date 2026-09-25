package panel

import (
	"encoding/base64"
	"fmt"
	"strings"

	"rsc.io/qr"
)

// qrDataURL draws text as a QR code in an SVG data URL. It is made on the
// server so the interface needs no QR library, and the secret it carries
// never passes through third-party code in the browser.
func qrDataURL(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4 // the blank border scanners expect
	n := code.Size + 2*quiet
	var path strings.Builder
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#fff"/><path d="%s" fill="#000"/></svg>`, n, n, path.String())
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg)), nil
}
