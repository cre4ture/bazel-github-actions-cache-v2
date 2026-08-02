package server

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var (
	errIncompleteActionResult = errors.New("action result CAS closure is incomplete")
	errInvalidActionResult    = errors.New("action result is invalid")
)

type actionResultValidator struct {
	server               *Server
	context              context.Context
	requirePersisted     bool
	validatedObjects     int
	validatedBlobs       map[string]int64
	validatedTrees       map[string]int64
	validatedDirectories map[string]int64
}

func (s *Server) validateActionResult(ctx context.Context, object object) error {
	return s.validateActionResultClosure(ctx, object, false)
}

func (s *Server) validateActionResultForPublication(ctx context.Context, object object) error {
	return s.validateActionResultClosure(ctx, object, true)
}

func (s *Server) validateActionResultClosure(
	ctx context.Context,
	object object,
	requirePersisted bool,
) error {
	data, err := os.ReadFile(object.path)
	if err != nil {
		return fmt.Errorf("read action result: %w", err)
	}
	references, err := parseActionResult(data)
	if err != nil {
		return fmt.Errorf("%w: %v", errInvalidActionResult, err)
	}
	validator := &actionResultValidator{
		server:               s,
		context:              ctx,
		requirePersisted:     requirePersisted,
		validatedBlobs:       make(map[string]int64),
		validatedTrees:       make(map[string]int64),
		validatedDirectories: make(map[string]int64),
	}
	for _, reference := range references.blobs.values {
		if err := validator.validateBlob(reference); err != nil {
			return err
		}
	}
	for _, reference := range references.trees.values {
		if err := validator.validateTree(reference); err != nil {
			return err
		}
	}
	return validator.validateDirectoryClosure(references.directories.values)
}

func (v *actionResultValidator) validateBlob(reference digestReference) error {
	if isImplicitEmptyDigest(reference) {
		return nil
	}
	if alreadyValidated, err := rememberDigest(v.validatedBlobs, reference); err != nil {
		return fmt.Errorf("%w: %v", errInvalidActionResult, err)
	} else if alreadyValidated {
		return nil
	}
	if err := v.reserveObject(); err != nil {
		return err
	}
	key := v.server.cfg.KeyPrefix + "-cas-" + reference.hash
	found, err := v.casExists(key)
	if err != nil {
		return err
	}
	if !found {
		return missingCASObject(reference)
	}
	return nil
}

func (v *actionResultValidator) validateTree(reference digestReference) error {
	if alreadyValidated, err := rememberDigest(v.validatedTrees, reference); err != nil {
		return fmt.Errorf("%w: %v", errInvalidActionResult, err)
	} else if alreadyValidated {
		return nil
	}
	object, err := v.loadCAS(reference)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(object.path)
	if err != nil {
		return fmt.Errorf("read tree CAS object %s: %w", reference.hash, err)
	}
	files, err := parseTree(data)
	if err != nil {
		return fmt.Errorf("%w: tree %s: %v", errInvalidActionResult, reference.hash, err)
	}
	for _, file := range files {
		if err := v.validateBlob(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *actionResultValidator) validateDirectoryClosure(roots []digestReference) error {
	queue := append([]digestReference(nil), roots...)
	for len(queue) > 0 {
		reference := queue[0]
		queue = queue[1:]
		if alreadyValidated, err := rememberDigest(v.validatedDirectories, reference); err != nil {
			return fmt.Errorf("%w: %v", errInvalidActionResult, err)
		} else if alreadyValidated {
			continue
		}
		object, err := v.loadCAS(reference)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(object.path)
		if err != nil {
			return fmt.Errorf("read directory CAS object %s: %w", reference.hash, err)
		}
		directory, err := parseDirectory(data)
		if err != nil {
			return fmt.Errorf("%w: directory %s: %v", errInvalidActionResult, reference.hash, err)
		}
		for _, file := range directory.files {
			if err := v.validateBlob(file); err != nil {
				return err
			}
		}
		queue = append(queue, directory.directories...)
	}
	return nil
}

func (v *actionResultValidator) loadCAS(reference digestReference) (object, error) {
	if err := v.reserveObject(); err != nil {
		return object{}, err
	}
	key := v.server.cfg.KeyPrefix + "-cas-" + reference.hash
	if v.requirePersisted {
		found, err := v.server.backendExists(v.context, key)
		if err != nil {
			return object{}, err
		}
		if !found {
			return object{}, missingCASObject(reference)
		}
	}
	cachedObject, found, err := v.server.resolve(v.context, key, "cas", reference.hash)
	if err != nil {
		return object{}, err
	}
	if !found {
		return object{}, missingCASObject(reference)
	}
	if cachedObject.size != reference.size {
		return object{}, fmt.Errorf(
			"%w: CAS object %s has size %d; action result declares %d",
			errInvalidActionResult,
			reference.hash,
			cachedObject.size,
			reference.size,
		)
	}
	return cachedObject, nil
}

func (v *actionResultValidator) casExists(key string) (bool, error) {
	if v.requirePersisted {
		return v.server.backendExists(v.context, key)
	}
	return v.server.exists(v.context, key)
}

func (v *actionResultValidator) reserveObject() error {
	v.validatedObjects++
	if v.validatedObjects <= maxActionResultObjects {
		return nil
	}
	return fmt.Errorf(
		"%w: action result references more than %d CAS objects",
		errInvalidActionResult,
		maxActionResultObjects,
	)
}

func missingCASObject(reference digestReference) error {
	return fmt.Errorf(
		"%w: missing CAS object %s/%d",
		errIncompleteActionResult,
		reference.hash,
		reference.size,
	)
}

func rememberDigest(validated map[string]int64, reference digestReference) (bool, error) {
	size, exists := validated[reference.hash]
	if !exists {
		validated[reference.hash] = reference.size
		return false, nil
	}
	if size != reference.size {
		return false, fmt.Errorf(
			"digest %s has inconsistent sizes %d and %d",
			reference.hash,
			size,
			reference.size,
		)
	}
	return true, nil
}
