package server

import (
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

func parseDigest(data []byte) (digestReference, error) {
	var hash string
	var size int64
	var hashSeen bool
	var sizeSeen bool
	err := visitWireFields(data, func(field wireField) error {
		switch field.number {
		case 1:
			value, err := field.message("digest.hash")
			if err != nil {
				return err
			}
			if hashSeen {
				return errors.New("digest.hash is repeated")
			}
			hashSeen = true
			hash = string(value)
		case 2:
			if field.wireType != protowire.VarintType {
				return errors.New("digest.size_bytes has the wrong wire type")
			}
			if sizeSeen {
				return errors.New("digest.size_bytes is repeated")
			}
			if field.varintValue > math.MaxInt64 {
				return errors.New("digest.size_bytes is negative or overflows int64")
			}
			sizeSeen = true
			size = int64(field.varintValue)
		}
		return nil
	})
	if err != nil {
		return digestReference{}, err
	}
	if !hashSeen || !digestPattern.MatchString(hash) {
		return digestReference{}, errors.New("digest.hash is not a lowercase SHA-256 digest")
	}
	return digestReference{hash: hash, size: size}, nil
}

type wireField struct {
	number      protowire.Number
	wireType    protowire.Type
	bytesValue  []byte
	varintValue uint64
}

func (f wireField) message(name string) ([]byte, error) {
	if f.wireType != protowire.BytesType {
		return nil, fmt.Errorf("%s has the wrong wire type", name)
	}
	return f.bytesValue, nil
}

func visitWireFields(data []byte, visit func(wireField) error) error {
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed < 0 {
			return protowire.ParseError(consumed)
		}
		if number < 1 {
			return errors.New("protobuf field number must be positive")
		}
		data = data[consumed:]

		field := wireField{number: number, wireType: wireType}
		switch wireType {
		case protowire.BytesType:
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return protowire.ParseError(n)
			}
			field.bytesValue = value
			consumed = n
		case protowire.VarintType:
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return protowire.ParseError(n)
			}
			field.varintValue = value
			consumed = n
		default:
			consumed = protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return protowire.ParseError(consumed)
			}
		}

		if err := visit(field); err != nil {
			return err
		}
		data = data[consumed:]
	}
	return nil
}
