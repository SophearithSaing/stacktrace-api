package app

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// ID is a canonical UUID string, independent of JSON and database drivers.
type ID string

func NewID() ID {
	var value [16]byte
	rand.Read(value[:])
	value[6] = value[6]&0x0f | 0x40 // UUID version 4.
	value[8] = value[8]&0x3f | 0x80 // RFC 9562 variant.
	encoded := hex.EncodeToString(value[:])
	return ID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

func ParseID(raw string) (ID, error) {
	if len(raw) != 36 || raw[8] != '-' || raw[13] != '-' || raw[18] != '-' || raw[23] != '-' {
		return "", ErrInvalidID
	}
	encoded := strings.ReplaceAll(raw, "-", "")
	if len(encoded) != 32 {
		return "", ErrInvalidID
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", ErrInvalidID
	}
	return ID(strings.ToLower(raw)), nil
}
