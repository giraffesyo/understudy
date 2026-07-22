package embedded

import (
	"bytes"
	"compress/gzip"
	"io"
)

// gunzip decompresses a gzipped byte slice.
func gunzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// GunzipReader returns a streaming reader over gzipped data (for uploads).
func GunzipReader(data []byte) (io.ReadCloser, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return r, nil
}
