// Package siteassets provides a small, validated filesystem store for site branding images.
package siteassets

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidAsset identifies rejected caller-provided asset content. Storage
// and filesystem failures deliberately do not use this sentinel so HTTP
// callers never report an infrastructure fault as a client error.
var ErrInvalidAsset = errors.New("invalid site asset")

type Limits struct{ MaxBytes, MaxPixels int64 }
type Asset struct {
	Key, ContentType    string
	Size, Width, Height int64
}
type Store struct {
	root   string
	limits Limits
}

func New(root string, limits Limits) (*Store, error) {
	if limits.MaxBytes <= 0 || limits.MaxPixels <= 0 {
		return nil, errors.New("site asset limits must be positive")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat site assets root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("site assets root is not a directory")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	return &Store{root: root, limits: limits}, nil
}
func (s *Store) Put(filename string, source io.Reader) (Asset, error) {
	ext, err := safeExtension(filename)
	if err != nil {
		return Asset{}, fmt.Errorf("%w: %v", ErrInvalidAsset, err)
	}
	dir := filepath.Join(s.root, "site")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Asset{}, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return Asset{}, err
	}
	temp, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return Asset{}, err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return Asset{}, err
	}
	n, err := io.Copy(temp, io.LimitReader(source, s.limits.MaxBytes+1))
	if err != nil {
		temp.Close()
		return Asset{}, err
	}
	if n > s.limits.MaxBytes {
		temp.Close()
		return Asset{}, fmt.Errorf("%w: site asset exceeds size limit", ErrInvalidAsset)
	}
	if err := temp.Close(); err != nil {
		return Asset{}, err
	}
	width, height, mime, err := validateImage(name, ext, s.limits.MaxPixels)
	if err != nil {
		return Asset{}, fmt.Errorf("%w: %v", ErrInvalidAsset, err)
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Asset{}, err
	}
	key := "site/" + hex.EncodeToString(random[:]) + "." + ext
	target := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.Rename(name, target); err != nil {
		return Asset{}, err
	}
	if err := os.Chmod(target, 0600); err != nil {
		return Asset{}, err
	}
	return Asset{Key: key, ContentType: mime, Size: n, Width: width, Height: height}, nil
}
func (s *Store) Open(key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, errors.New("invalid site asset key")
	}
	return os.Open(filepath.Join(s.root, filepath.FromSlash(key)))
}

// Remove deletes only a validated opaque key. It is deliberately separate
// from Put so callers can couple deletion to durable metadata cleanup.
func (s *Store) Remove(key string) error {
	if !validKey(key) {
		return errors.New("invalid site asset key")
	}
	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(key)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func safeExtension(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return "", errors.New("invalid site asset filename")
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ext == "jpg" {
		ext = "jpeg"
	}
	if ext != "png" && ext != "jpeg" && ext != "gif" && ext != "ico" {
		return "", errors.New("unsupported site asset type")
	}
	return ext, nil
}
func validateImage(name, ext string, maxPixels int64) (int64, int64, string, error) {
	if ext == "ico" {
		f, err := os.Open(name)
		if err != nil {
			return 0, 0, "", err
		}
		defer f.Close()
		width, height, err := icoDimensions(f)
		if err != nil {
			return 0, 0, "", errors.New("site asset is not a valid ICO image")
		}
		if width*height > maxPixels {
			return 0, 0, "", errors.New("site asset exceeds pixel limit")
		}
		return width, height, "image/x-icon", nil
	}
	f, err := os.Open(name)
	if err != nil {
		return 0, 0, "", err
	}
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		return 0, 0, "", errors.New("site asset is not a supported raster image")
	}
	mime, ok := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if !ok || extensionForFormat(ext) != format {
		return 0, 0, "", errors.New("site asset extension does not match image content")
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if cfg.Width <= 0 || cfg.Height <= 0 || pixels <= 0 || pixels > maxPixels {
		return 0, 0, "", errors.New("site asset exceeds pixel limit")
	}
	return int64(cfg.Width), int64(cfg.Height), mime, nil
}

// icoDimensions validates ICO headers and entries before accepting the file.
func icoDimensions(r io.ReadSeeker) (int64, int64, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil || header[0] != 0 || header[1] != 0 || header[2] != 1 || header[3] != 0 {
		return 0, 0, errors.New("invalid ICO header")
	}
	count := int(header[4]) | int(header[5])<<8
	if count < 1 {
		return 0, 0, errors.New("empty ICO directory")
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil || int64(6+16*count) > size {
		return 0, 0, errors.New("invalid ICO directory")
	}
	if _, err := r.Seek(6, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var maxWidth, maxHeight int64
	for entryIndex := range count {
		var entry [16]byte
		if _, err := io.ReadFull(r, entry[:]); err != nil {
			return 0, 0, err
		}
		width, height := int64(entry[0]), int64(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		bytes := int64(uint32(entry[8]) | uint32(entry[9])<<8 | uint32(entry[10])<<16 | uint32(entry[11])<<24)
		offset := int64(uint32(entry[12]) | uint32(entry[13])<<8 | uint32(entry[14])<<16 | uint32(entry[15])<<24)
		if bytes == 0 || offset < int64(6+16*count) || offset > size || bytes > size-offset {
			return 0, 0, errors.New("invalid ICO image entry")
		}
		payload := make([]byte, bytes)
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return 0, 0, err
		}
		if _, err := io.ReadFull(r, payload); err != nil || !validICOImagePayload(payload, width, height) {
			return 0, 0, errors.New("invalid ICO image payload")
		}
		if _, err := r.Seek(int64(6+16*(entryIndex+1)), io.SeekStart); err != nil {
			return 0, 0, err
		}
		if width > maxWidth {
			maxWidth = width
		}
		if height > maxHeight {
			maxHeight = height
		}
	}
	return maxWidth, maxHeight, nil
}

func validICOImagePayload(payload []byte, width, height int64) bool {
	// An entry may contain a PNG image, otherwise require a BITMAPINFOHEADER
	// with matching dimensions and a sane image layout.
	if len(payload) >= 24 && bytes.Equal(payload[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		w := int64(uint32(payload[16])<<24 | uint32(payload[17])<<16 | uint32(payload[18])<<8 | uint32(payload[19]))
		h := int64(uint32(payload[20])<<24 | uint32(payload[21])<<16 | uint32(payload[22])<<8 | uint32(payload[23]))
		return w == width && h == height
	}
	if len(payload) < 40 || payload[0] != 40 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		return false
	}
	w := int64(uint32(payload[4]) | uint32(payload[5])<<8 | uint32(payload[6])<<16 | uint32(payload[7])<<24)
	h := int64(uint32(payload[8]) | uint32(payload[9])<<8 | uint32(payload[10])<<16 | uint32(payload[11])<<24)
	return w == width && h == height*2 && payload[12] == 1 && payload[13] == 0
}

func extensionForFormat(ext string) string {
	if ext == "jpeg" {
		return "jpeg"
	}
	return ext
}
func validKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || parts[0] != "site" {
		return false
	}
	stem := strings.TrimSuffix(parts[1], filepath.Ext(parts[1]))
	if len(stem) != 64 {
		return false
	}
	for _, c := range stem {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	_, err := safeExtension(parts[1])
	return err == nil
}
