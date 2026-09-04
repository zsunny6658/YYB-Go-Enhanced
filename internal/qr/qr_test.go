package qr

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateQRCodeImage(t *testing.T) {
	// 1x1 transparent PNG. Keeping the fixture inline makes this test independent
	// from the network and from files in the resource directory.
	const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	data, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateQRCodeImage(data); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if err := validateQRCodeImage([]byte("<html>blocked</html>")); err == nil {
		t.Fatal("HTML response was accepted as a QR image")
	}
}

func TestDataURIImageUsesDetectedMimeType(t *testing.T) {
	const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	data, _ := base64.StdEncoding.DecodeString(pngBase64)
	uri := DataURIImage(data)
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("unexpected data URI prefix: %s", uri)
	}
	if !bytes.Contains([]byte(uri), []byte(pngBase64)) {
		t.Fatal("data URI does not contain the encoded image")
	}
}
