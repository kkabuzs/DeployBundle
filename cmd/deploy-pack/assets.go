package main

import (
	"bytes"
	"compress/gzip"

	"github.com/openai/deploybundle/internal/assets"
)

// Configuration assets are bounded snapshots, so compressing in memory avoids
// plaintext staging files and races with source files changing during packing.
func saveAssets(output string, sources []assets.Source, compression string, key []byte, chunkSize int) error {
	var archive bytes.Buffer
	if compression == "gzip" {
		gz := gzip.NewWriter(&archive)
		if err := assets.WriteTar(gz, sources); err != nil {
			return err
		}
		if err := gz.Close(); err != nil {
			return err
		}
	} else if err := assets.WriteTar(&archive, sources); err != nil {
		return err
	}
	return encryptBytes(output, archive.Bytes(), key, chunkSize)
}
