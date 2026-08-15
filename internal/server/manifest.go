package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/ipfs/go-cid"
	ipld "github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/datamodel"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/multiformats/go-multihash"
)

const manifestFormatVersion = 1

type manifest struct {
	Version int64
	Parents []string
	Packs   []packDescriptor
	CAS     []manifestObject
	Actions []manifestAction
}

type packDescriptor struct {
	ID   string
	Key  string
	Size int64
}

type manifestObject struct {
	Digest string
	CID    string
	PackID string
	Size   int64
}

type manifestAction struct {
	Digest       string
	CID          string
	PackID       string
	Size         int64
	ClosurePacks []string
}

func encodeManifest(value manifest) ([]byte, string, error) {
	node, err := manifestToNode(value)
	if err != nil {
		return nil, "", err
	}
	var encoded bytes.Buffer
	if err := dagcbor.Encode(node, &encoded); err != nil {
		return nil, "", fmt.Errorf("encode DAG-CBOR manifest: %w", err)
	}
	data := encoded.Bytes()
	manifestCID, err := dagCBORCID(data)
	if err != nil {
		return nil, "", err
	}
	return data, manifestCID.String(), nil
}

func decodeManifest(data []byte, expectedCID string) (manifest, error) {
	if expectedCID != "" {
		actual, err := dagCBORCID(data)
		if err != nil {
			return manifest{}, err
		}
		if actual.String() != expectedCID {
			return manifest{}, fmt.Errorf("manifest CID mismatch: expected %s, got %s", expectedCID, actual)
		}
	}
	node, err := ipld.Decode(data, dagcbor.Decode)
	if err != nil {
		return manifest{}, fmt.Errorf("decode DAG-CBOR manifest: %w", err)
	}
	return nodeToManifest(node)
}

func manifestToNode(value manifest) (datamodel.Node, error) {
	if value.Version != manifestFormatVersion {
		return nil, fmt.Errorf("unsupported manifest version %d", value.Version)
	}
	parents := append([]string(nil), value.Parents...)
	sort.Strings(parents)
	packs := append([]packDescriptor(nil), value.Packs...)
	sort.Slice(packs, func(i, j int) bool { return packs[i].ID < packs[j].ID })
	cas := append([]manifestObject(nil), value.CAS...)
	sort.Slice(cas, func(i, j int) bool { return cas[i].Digest < cas[j].Digest })
	actions := append([]manifestAction(nil), value.Actions...)
	sort.Slice(actions, func(i, j int) bool { return actions[i].Digest < actions[j].Digest })

	builder := basicnode.Prototype.Map.NewBuilder()
	assembler, err := builder.BeginMap(5)
	if err != nil {
		return nil, err
	}
	if err := assembleManifestActions(assembler, actions); err != nil {
		return nil, err
	}
	if err := assembleManifestCAS(assembler, cas); err != nil {
		return nil, err
	}
	if err := assembleManifestPacks(assembler, packs); err != nil {
		return nil, err
	}
	if err := assembleStringList(assembler, "parents", parents); err != nil {
		return nil, err
	}
	if err := assembleInt(assembler, "version", value.Version); err != nil {
		return nil, err
	}
	if err := assembler.Finish(); err != nil {
		return nil, err
	}
	return builder.Build(), nil
}

func assembleManifestActions(assembler datamodel.MapAssembler, actions []manifestAction) error {
	if err := assembler.AssembleKey().AssignString("actions"); err != nil {
		return err
	}
	list, err := assembler.AssembleValue().BeginList(int64(len(actions)))
	if err != nil {
		return err
	}
	for _, action := range actions {
		closure := append([]string(nil), action.ClosurePacks...)
		sort.Strings(closure)
		mapAssembler, err := list.AssembleValue().BeginMap(5)
		if err != nil {
			return err
		}
		for _, field := range []struct{ key, value string }{
			{"cid", action.CID},
			{"digest", action.Digest},
			{"pack", action.PackID},
		} {
			if err := assembleString(mapAssembler, field.key, field.value); err != nil {
				return err
			}
		}
		if err := assembleStringList(mapAssembler, "closure", closure); err != nil {
			return err
		}
		if err := assembleInt(mapAssembler, "size", action.Size); err != nil {
			return err
		}
		if err := mapAssembler.Finish(); err != nil {
			return err
		}
	}
	return list.Finish()
}

func assembleManifestCAS(assembler datamodel.MapAssembler, objects []manifestObject) error {
	if err := assembler.AssembleKey().AssignString("cas"); err != nil {
		return err
	}
	list, err := assembler.AssembleValue().BeginList(int64(len(objects)))
	if err != nil {
		return err
	}
	for _, object := range objects {
		mapAssembler, err := list.AssembleValue().BeginMap(4)
		if err != nil {
			return err
		}
		for _, field := range []struct{ key, value string }{
			{"cid", object.CID},
			{"digest", object.Digest},
			{"pack", object.PackID},
		} {
			if err := assembleString(mapAssembler, field.key, field.value); err != nil {
				return err
			}
		}
		if err := assembleInt(mapAssembler, "size", object.Size); err != nil {
			return err
		}
		if err := mapAssembler.Finish(); err != nil {
			return err
		}
	}
	return list.Finish()
}

func assembleManifestPacks(assembler datamodel.MapAssembler, packs []packDescriptor) error {
	if err := assembler.AssembleKey().AssignString("packs"); err != nil {
		return err
	}
	list, err := assembler.AssembleValue().BeginList(int64(len(packs)))
	if err != nil {
		return err
	}
	for _, pack := range packs {
		mapAssembler, err := list.AssembleValue().BeginMap(3)
		if err != nil {
			return err
		}
		if err := assembleString(mapAssembler, "id", pack.ID); err != nil {
			return err
		}
		if err := assembleString(mapAssembler, "key", pack.Key); err != nil {
			return err
		}
		if err := assembleInt(mapAssembler, "size", pack.Size); err != nil {
			return err
		}
		if err := mapAssembler.Finish(); err != nil {
			return err
		}
	}
	return list.Finish()
}

func assembleStringList(assembler datamodel.MapAssembler, key string, values []string) error {
	if err := assembler.AssembleKey().AssignString(key); err != nil {
		return err
	}
	list, err := assembler.AssembleValue().BeginList(int64(len(values)))
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := list.AssembleValue().AssignString(value); err != nil {
			return err
		}
	}
	return list.Finish()
}

func assembleString(assembler datamodel.MapAssembler, key, value string) error {
	if err := assembler.AssembleKey().AssignString(key); err != nil {
		return err
	}
	return assembler.AssembleValue().AssignString(value)
}

func assembleInt(assembler datamodel.MapAssembler, key string, value int64) error {
	if err := assembler.AssembleKey().AssignString(key); err != nil {
		return err
	}
	return assembler.AssembleValue().AssignInt(value)
}

func nodeToManifest(node datamodel.Node) (manifest, error) {
	if node.Kind() != datamodel.Kind_Map {
		return manifest{}, errors.New("manifest must be a map")
	}
	version, err := nodeInt(node, "version")
	if err != nil {
		return manifest{}, err
	}
	if version != manifestFormatVersion {
		return manifest{}, fmt.Errorf("unsupported manifest version %d", version)
	}
	parents, err := nodeStringList(node, "parents")
	if err != nil {
		return manifest{}, err
	}
	packs, err := nodePacks(node)
	if err != nil {
		return manifest{}, err
	}
	cas, err := nodeCAS(node)
	if err != nil {
		return manifest{}, err
	}
	actions, err := nodeActions(node)
	if err != nil {
		return manifest{}, err
	}
	value := manifest{Version: version, Parents: parents, Packs: packs, CAS: cas, Actions: actions}
	if err := value.validate(); err != nil {
		return manifest{}, err
	}
	return value, nil
}

func (value manifest) validate() error {
	if len(value.Parents) > 4_096 || len(value.Packs) > 4_096 || len(value.CAS) > 100_000 || len(value.Actions) > 100_000 {
		return errors.New("manifest exceeds entry limit")
	}
	packIDs := make(map[string]struct{}, len(value.Packs))
	for _, pack := range value.Packs {
		if !digestPattern.MatchString(pack.ID) || pack.Size <= 0 || pack.Key == "" || pack.Key != packKeyFor("", pack.ID) && !stringsHasSuffix(pack.Key, "-car-pack-v1-"+pack.ID) {
			return errors.New("manifest has invalid pack descriptor")
		}
		if _, duplicate := packIDs[pack.ID]; duplicate {
			return errors.New("manifest repeats a pack descriptor")
		}
		packIDs[pack.ID] = struct{}{}
	}
	seenCAS := make(map[string]struct{}, len(value.CAS))
	for _, object := range value.CAS {
		if !digestPattern.MatchString(object.Digest) || object.Size < 0 || !validRawCID(object.CID, object.Digest) {
			return errors.New("manifest has invalid CAS entry")
		}
		if _, ok := packIDs[object.PackID]; !ok {
			return errors.New("CAS entry references an undeclared pack")
		}
		if _, duplicate := seenCAS[object.Digest]; duplicate {
			return errors.New("manifest repeats a CAS digest")
		}
		seenCAS[object.Digest] = struct{}{}
	}
	seenActions := make(map[string]struct{}, len(value.Actions))
	for _, action := range value.Actions {
		if !digestPattern.MatchString(action.Digest) || action.Size < 0 || !validCID(action.CID) {
			return errors.New("manifest has invalid action entry")
		}
		if _, ok := packIDs[action.PackID]; !ok {
			return errors.New("action entry references an undeclared pack")
		}
		for _, packID := range action.ClosurePacks {
			if _, ok := packIDs[packID]; !ok && !digestPattern.MatchString(packID) {
				return errors.New("action closure has invalid pack ID")
			}
		}
		if _, duplicate := seenActions[action.Digest]; duplicate {
			return errors.New("manifest repeats an action digest")
		}
		seenActions[action.Digest] = struct{}{}
	}
	for _, parent := range value.Parents {
		if !validCID(parent) {
			return errors.New("manifest has invalid parent CID")
		}
	}
	return nil
}

func nodePacks(node datamodel.Node) ([]packDescriptor, error) {
	list, err := node.LookupByString("packs")
	if err != nil || list.Kind() != datamodel.Kind_List {
		return nil, errors.New("manifest packs must be a list")
	}
	values := make([]packDescriptor, 0, list.Length())
	iterator := list.ListIterator()
	for !iterator.Done() {
		_, item, err := iterator.Next()
		if err != nil || item.Kind() != datamodel.Kind_Map {
			return nil, errors.New("manifest pack must be a map")
		}
		id, err := nodeString(item, "id")
		if err != nil {
			return nil, err
		}
		key, err := nodeString(item, "key")
		if err != nil {
			return nil, err
		}
		size, err := nodeInt(item, "size")
		if err != nil {
			return nil, err
		}
		values = append(values, packDescriptor{ID: id, Key: key, Size: size})
	}
	return values, nil
}

func nodeCAS(node datamodel.Node) ([]manifestObject, error) {
	list, err := node.LookupByString("cas")
	if err != nil || list.Kind() != datamodel.Kind_List {
		return nil, errors.New("manifest cas must be a list")
	}
	values := make([]manifestObject, 0, list.Length())
	iterator := list.ListIterator()
	for !iterator.Done() {
		_, item, err := iterator.Next()
		if err != nil || item.Kind() != datamodel.Kind_Map {
			return nil, errors.New("manifest CAS entry must be a map")
		}
		value, err := nodeObject(item)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func nodeActions(node datamodel.Node) ([]manifestAction, error) {
	list, err := node.LookupByString("actions")
	if err != nil || list.Kind() != datamodel.Kind_List {
		return nil, errors.New("manifest actions must be a list")
	}
	values := make([]manifestAction, 0, list.Length())
	iterator := list.ListIterator()
	for !iterator.Done() {
		_, item, err := iterator.Next()
		if err != nil || item.Kind() != datamodel.Kind_Map {
			return nil, errors.New("manifest action entry must be a map")
		}
		digest, err := nodeString(item, "digest")
		if err != nil {
			return nil, err
		}
		contentCID, err := nodeString(item, "cid")
		if err != nil {
			return nil, err
		}
		packID, err := nodeString(item, "pack")
		if err != nil {
			return nil, err
		}
		size, err := nodeInt(item, "size")
		if err != nil {
			return nil, err
		}
		closure, err := nodeStringList(item, "closure")
		if err != nil {
			return nil, err
		}
		values = append(values, manifestAction{Digest: digest, CID: contentCID, PackID: packID, Size: size, ClosurePacks: closure})
	}
	return values, nil
}

func nodeObject(node datamodel.Node) (manifestObject, error) {
	digest, err := nodeString(node, "digest")
	if err != nil {
		return manifestObject{}, err
	}
	contentCID, err := nodeString(node, "cid")
	if err != nil {
		return manifestObject{}, err
	}
	packID, err := nodeString(node, "pack")
	if err != nil {
		return manifestObject{}, err
	}
	size, err := nodeInt(node, "size")
	if err != nil {
		return manifestObject{}, err
	}
	return manifestObject{Digest: digest, CID: contentCID, PackID: packID, Size: size}, nil
}

func nodeString(node datamodel.Node, key string) (string, error) {
	value, err := node.LookupByString(key)
	if err != nil || value.Kind() != datamodel.Kind_String {
		return "", fmt.Errorf("manifest %s must be a string", key)
	}
	return value.AsString()
}

func nodeInt(node datamodel.Node, key string) (int64, error) {
	value, err := node.LookupByString(key)
	if err != nil || value.Kind() != datamodel.Kind_Int {
		return 0, fmt.Errorf("manifest %s must be an integer", key)
	}
	return value.AsInt()
}

func nodeStringList(node datamodel.Node, key string) ([]string, error) {
	list, err := node.LookupByString(key)
	if err != nil || list.Kind() != datamodel.Kind_List {
		return nil, fmt.Errorf("manifest %s must be a list", key)
	}
	values := make([]string, 0, list.Length())
	iterator := list.ListIterator()
	for !iterator.Done() {
		_, item, err := iterator.Next()
		if err != nil || item.Kind() != datamodel.Kind_String {
			return nil, fmt.Errorf("manifest %s must contain strings", key)
		}
		value, err := item.AsString()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func dagCBORCID(data []byte) (cid.Cid, error) {
	prefix := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: multihash.SHA2_256, MhLength: -1}
	return prefix.Sum(data)
}

func rawCIDForDigest(digest string) (cid.Cid, error) {
	if !digestPattern.MatchString(digest) {
		return cid.Undef, errors.New("invalid SHA-256 digest")
	}
	bytes, err := hex.DecodeString(digest)
	if err != nil {
		return cid.Undef, err
	}
	hash, err := multihash.Encode(bytes, multihash.SHA2_256)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(cid.Raw, hash), nil
}

func rawCIDForData(data []byte) (cid.Cid, error) {
	sum := sha256.Sum256(data)
	return rawCIDForDigest(hex.EncodeToString(sum[:]))
}

func validRawCID(value, digest string) bool {
	expected, err := rawCIDForDigest(digest)
	return err == nil && value == expected.String()
}

func validCID(value string) bool {
	_, err := cid.Decode(value)
	return err == nil
}

func packKeyFor(prefix, id string) string {
	return prefix + "-car-pack-v1-" + id
}

func manifestKeyFor(prefix, id string) string {
	return prefix + "-car-manifest-v1-" + id
}

func stringsHasSuffix(value, suffix string) bool {
	return len(value) >= len(suffix) && value[len(value)-len(suffix):] == suffix
}
