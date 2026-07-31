package server

import (
	"errors"
	"fmt"
)

const maxActionResultObjects = 100_000

type digestReference struct {
	hash string
	size int64
}

type digestCollection struct {
	values []digestReference
	sizes  map[string]int64
}

func newDigestCollection() digestCollection {
	return digestCollection{sizes: make(map[string]int64)}
}

func (c *digestCollection) add(reference digestReference) error {
	if existingSize, exists := c.sizes[reference.hash]; exists {
		if existingSize != reference.size {
			return fmt.Errorf(
				"digest %s has inconsistent sizes %d and %d",
				reference.hash,
				existingSize,
				reference.size,
			)
		}
		return nil
	}
	c.sizes[reference.hash] = reference.size
	c.values = append(c.values, reference)
	return nil
}

type actionResultReferences struct {
	blobs       digestCollection
	trees       digestCollection
	directories digestCollection
}

func newActionResultReferences() actionResultReferences {
	return actionResultReferences{
		blobs:       newDigestCollection(),
		trees:       newDigestCollection(),
		directories: newDigestCollection(),
	}
}

func parseActionResult(data []byte) (actionResultReferences, error) {
	references := newActionResultReferences()
	var stdoutDigest *digestReference
	var stderrDigest *digestReference
	var stdoutInline bool
	var stderrInline bool

	err := visitWireFields(data, func(field wireField) error {
		switch field.number {
		case 2:
			message, err := field.message("action_result.output_files")
			if err != nil {
				return err
			}
			digest, inline, err := parseOutputFile(message)
			if err != nil {
				return err
			}
			if digest != nil && !inline {
				return references.blobs.add(*digest)
			}
		case 3:
			message, err := field.message("action_result.output_directories")
			if err != nil {
				return err
			}
			tree, root, err := parseOutputDirectory(message)
			if err != nil {
				return err
			}
			if tree != nil {
				if err := references.trees.add(*tree); err != nil {
					return err
				}
			}
			if root != nil {
				if err := references.directories.add(*root); err != nil {
					return err
				}
			}
		case 5:
			if _, err := field.message("action_result.stdout_raw"); err != nil {
				return err
			}
			stdoutInline = true
		case 6:
			message, err := field.message("action_result.stdout_digest")
			if err != nil {
				return err
			}
			reference, err := parseDigest(message)
			if err != nil {
				return fmt.Errorf("action_result.stdout_digest: %w", err)
			}
			stdoutDigest = &reference
		case 7:
			if _, err := field.message("action_result.stderr_raw"); err != nil {
				return err
			}
			stderrInline = true
		case 8:
			message, err := field.message("action_result.stderr_digest")
			if err != nil {
				return err
			}
			reference, err := parseDigest(message)
			if err != nil {
				return fmt.Errorf("action_result.stderr_digest: %w", err)
			}
			stderrDigest = &reference
		}
		return nil
	})
	if err != nil {
		return actionResultReferences{}, err
	}
	if stdoutDigest != nil && !stdoutInline {
		if err := references.blobs.add(*stdoutDigest); err != nil {
			return actionResultReferences{}, err
		}
	}
	if stderrDigest != nil && !stderrInline {
		if err := references.blobs.add(*stderrDigest); err != nil {
			return actionResultReferences{}, err
		}
	}
	return references, nil
}

func parseOutputFile(data []byte) (*digestReference, bool, error) {
	var digest *digestReference
	var contentsInline bool
	err := visitWireFields(data, func(field wireField) error {
		switch field.number {
		case 2:
			message, err := field.message("output_file.digest")
			if err != nil {
				return err
			}
			reference, err := parseDigest(message)
			if err != nil {
				return fmt.Errorf("output_file.digest: %w", err)
			}
			if digest != nil {
				return errors.New("output_file.digest is repeated")
			}
			digest = &reference
		case 5:
			if _, err := field.message("output_file.contents"); err != nil {
				return err
			}
			contentsInline = true
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if digest == nil && !contentsInline {
		return nil, false, errors.New("output_file has neither digest nor inline contents")
	}
	return digest, contentsInline, nil
}

func parseOutputDirectory(data []byte) (*digestReference, *digestReference, error) {
	var tree *digestReference
	var root *digestReference
	err := visitWireFields(data, func(field wireField) error {
		var destination **digestReference
		var name string
		switch field.number {
		case 3:
			destination = &tree
			name = "output_directory.tree_digest"
		case 5:
			destination = &root
			name = "output_directory.root_directory_digest"
		default:
			return nil
		}
		message, err := field.message(name)
		if err != nil {
			return err
		}
		reference, err := parseDigest(message)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if *destination != nil {
			return fmt.Errorf("%s is repeated", name)
		}
		*destination = &reference
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if tree == nil && root == nil {
		return nil, nil, errors.New("output_directory has no tree or root directory digest")
	}
	return tree, root, nil
}
