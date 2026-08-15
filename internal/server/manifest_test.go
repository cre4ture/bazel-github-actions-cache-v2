package server

import (
	"bytes"
	"testing"
)

func TestManifestDAGCBORRoundTripIsDeterministic(t *testing.T) {
	casDigest := digest([]byte("cas"))
	casCID, err := rawCIDForDigest(casDigest)
	if err != nil {
		t.Fatal(err)
	}
	actionCID, err := rawCIDForData([]byte{0x08, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	packID := digest([]byte("pack"))
	value := manifest{
		Version: manifestFormatVersion,
		Parents: []string{},
		Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 123}},
		CAS:     []manifestObject{{Digest: casDigest, CID: casCID.String(), PackID: packID, Size: 3}},
		Actions: []manifestAction{{Digest: digest([]byte("action")), CID: actionCID.String(), PackID: packID, Size: 2, ClosurePacks: []string{packID}}},
	}
	encoded, manifestCID, err := encodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeManifest(encoded, manifestCID)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, secondCID, err := encodeManifest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if manifestCID != secondCID || !bytes.Equal(encoded, reencoded) {
		t.Fatal("DAG-CBOR manifest is not deterministic")
	}
}

func TestManifestRejectsTamperedCID(t *testing.T) {
	value := manifest{Version: manifestFormatVersion}
	encoded, manifestCID, err := encodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := decodeManifest(encoded, manifestCID); err == nil {
		t.Fatal("tampered manifest was accepted")
	}
}
