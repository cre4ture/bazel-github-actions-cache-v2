package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

type parsedDirectory struct {
	files       []digestReference
	directories []digestReference
}

func parseDirectory(data []byte) (parsedDirectory, error) {
	var directory parsedDirectory
	err := visitWireFields(data, func(field wireField) error {
		switch field.number {
		case 1:
			message, err := field.message("directory.files")
			if err != nil {
				return err
			}
			reference, err := parseNodeDigest(message, "file_node")
			if err != nil {
				return err
			}
			directory.files = append(directory.files, reference)
		case 2:
			message, err := field.message("directory.directories")
			if err != nil {
				return err
			}
			reference, err := parseNodeDigest(message, "directory_node")
			if err != nil {
				return err
			}
			directory.directories = append(directory.directories, reference)
		}
		return nil
	})
	return directory, err
}

func parseNodeDigest(data []byte, nodeName string) (digestReference, error) {
	var digest *digestReference
	err := visitWireFields(data, func(field wireField) error {
		if field.number != 2 {
			return nil
		}
		message, err := field.message(nodeName + ".digest")
		if err != nil {
			return err
		}
		reference, err := parseDigest(message)
		if err != nil {
			return fmt.Errorf("%s.digest: %w", nodeName, err)
		}
		if digest != nil {
			return fmt.Errorf("%s.digest is repeated", nodeName)
		}
		digest = &reference
		return nil
	})
	if err != nil {
		return digestReference{}, err
	}
	if digest == nil {
		return digestReference{}, fmt.Errorf("%s has no digest", nodeName)
	}
	return *digest, nil
}

func parseTree(data []byte) ([]digestReference, error) {
	var directories []parsedDirectory
	childDigests := make(map[string]int64)
	rootSeen := false

	err := visitWireFields(data, func(field wireField) error {
		if field.number != 1 && field.number != 2 {
			return nil
		}
		message, err := field.message("tree.directory")
		if err != nil {
			return err
		}
		if field.number == 1 {
			if rootSeen {
				return errors.New("tree.root is repeated")
			}
			rootSeen = true
		} else {
			sum := sha256.Sum256(message)
			hash := hex.EncodeToString(sum[:])
			if existingSize, exists := childDigests[hash]; exists && existingSize != int64(len(message)) {
				return fmt.Errorf("tree child %s has inconsistent sizes", hash)
			}
			childDigests[hash] = int64(len(message))
		}
		directory, err := parseDirectory(message)
		if err != nil {
			return fmt.Errorf("tree.directory: %w", err)
		}
		directories = append(directories, directory)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !rootSeen {
		return nil, errors.New("tree has no root directory")
	}

	files := newDigestCollection()
	for _, directory := range directories {
		for _, file := range directory.files {
			if err := files.add(file); err != nil {
				return nil, err
			}
		}
		for _, child := range directory.directories {
			size, exists := childDigests[child.hash]
			if !exists || size != child.size {
				return nil, fmt.Errorf(
					"tree references missing child directory %s/%d",
					child.hash,
					child.size,
				)
			}
		}
	}
	return files.values, nil
}
