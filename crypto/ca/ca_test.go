package ca

import (
	"encoding/hex"
	"fmt"
)

func fromHex256(hash string) (h [32]byte) {
	if len(hash) != hex.EncodedLen(len(h)) {
		panic("wrong length hash")
	}
	_, err := hex.Decode(h[:], []byte(hash))
	if err != nil {
		panic(err)
	}
	return h
}

func fromHex(input string) []byte {
	h := make([]byte, len(input)/2)
	_, err := hex.Decode(h[:], []byte(input))
	if err != nil {
		panic(err)
	}
	return h
}

func mustDecodePoint(b [32]byte) Point {
	p, ok := decodePoint(b)
	if !ok {
		panic(fmt.Sprintf("could not decode point %x", b[:]))
	}
	return p
}
