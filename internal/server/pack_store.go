package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-car/v2/blockstore"
)

// packStore implements the CARv2/manifest-DAG storage mode. It keeps
// uncommitted writes local, publishes a CARv2 pack first, and only then commits
// the corresponding immutable DAG-CBOR manifest. A manifest therefore never
// advertises data that a future runner cannot restore.
type packStore struct {
	server  *Server
	catalog cache.Catalog

	keyPrefix     string
	targetSize    int64
	flushInterval time.Duration
	maxManifests  int

	mu              sync.Mutex
	packs           map[string]packDescriptor
	cas             map[string]manifestObject
	actions         map[string]manifestAction
	actionConflicts map[string]struct{}
	heads           map[string]struct{}
	pendingCAS      map[string]object
	pendingCASOrder []string
	pendingActions  map[string]pendingAction
	pendingACOrder  []string
	pendingBytes    int64
	loadedPacks     map[string]string
	loadingPacks    map[string]*packLoad

	flushRequests chan struct{}
	stop          chan struct{}
	closeOnce     sync.Once
	workers       sync.WaitGroup
}

type pendingAction struct {
	object  object
	closure []digestReference
	cid     string
}

type packLoad struct {
	done chan struct{}
	path string
	err  error
}

func newPackStore(server *Server) (*packStore, error) {
	if server.cfg.Catalog == nil {
		return nil, errors.New("packed storage mode requires a manifest catalog")
	}
	packs := &packStore{
		server:          server,
		catalog:         server.cfg.Catalog,
		keyPrefix:       server.cfg.KeyPrefix,
		targetSize:      server.cfg.PackSize,
		flushInterval:   server.cfg.PackFlushInterval,
		maxManifests:    server.cfg.MaxManifests,
		packs:           make(map[string]packDescriptor),
		cas:             make(map[string]manifestObject),
		actions:         make(map[string]manifestAction),
		actionConflicts: make(map[string]struct{}),
		heads:           make(map[string]struct{}),
		pendingCAS:      make(map[string]object),
		pendingActions:  make(map[string]pendingAction),
		loadedPacks:     make(map[string]string),
		loadingPacks:    make(map[string]*packLoad),
		flushRequests:   make(chan struct{}, 1),
		stop:            make(chan struct{}),
	}
	context, cancel := context.WithTimeout(context.Background(), server.cfg.BackendTimeout)
	err := packs.discover(context)
	cancel()
	if err != nil {
		server.stats.manifestDiscoveryErrors.Add(1)
		if !server.cfg.FailOpen {
			return nil, err
		}
		server.cfg.Logger.Printf("packed-cache manifest discovery failed; starting empty: %s", safeError(err))
	}
	packs.workers.Add(1)
	go packs.flushWorker()
	return packs, nil
}

func (p *packStore) flushWorker() {
	defer p.workers.Done()
	ticker := time.NewTicker(p.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		case <-p.flushRequests:
		}
		context, cancel := context.WithTimeout(context.Background(), p.server.cfg.BackendTimeout)
		if err := p.flush(context, false); err != nil {
			p.server.stats.backendSaveErrors.Add(1)
			p.server.cfg.Logger.Printf("packed-cache flush failed: %s", safeError(err))
		}
		cancel()
	}
}

func (p *packStore) close(ctx context.Context) error {
	p.closeOnce.Do(func() { close(p.stop) })
	p.workers.Wait()
	return p.flush(ctx, true)
}

func (p *packStore) stageCAS(digest string, value object) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pendingCAS[digest]; exists {
		return
	}
	if _, exists := p.cas[digest]; exists {
		return
	}
	p.pendingCAS[digest] = value
	p.pendingCASOrder = append(p.pendingCASOrder, digest)
	p.pendingBytes += value.size
	p.requestFlushLocked()
}

func (p *packStore) stageAction(digest string, value object, closure []digestReference) error {
	data, err := os.ReadFile(value.path)
	if err != nil {
		return fmt.Errorf("read pending action result: %w", err)
	}
	contentCID, err := rawCIDForData(data)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, exists := p.pendingActions[digest]; exists {
		if existing.cid != contentCID.String() {
			// A single runner produced two different values for one action key.
			// Do not publish either value; silently choosing one would make a
			// non-deterministic action look cacheable.
			delete(p.pendingActions, digest)
			p.actionConflicts[digest] = struct{}{}
		}
		return nil
	}
	p.pendingActions[digest] = pendingAction{
		object:  value,
		closure: sortDigestReferences(closure),
		cid:     contentCID.String(),
	}
	p.pendingACOrder = append(p.pendingACOrder, digest)
	p.pendingBytes += value.size
	p.requestFlushLocked()
	return nil
}

func (p *packStore) requestFlushLocked() {
	if p.pendingBytes < p.targetSize {
		return
	}
	select {
	case p.flushRequests <- struct{}{}:
	default:
	}
}

func (p *packStore) resolve(ctx context.Context, key, kind, digest string) (object, bool, error) {
	p.server.objectsMu.RLock()
	value, exists := p.server.objects[key]
	p.server.objectsMu.RUnlock()
	if exists {
		return value, true, nil
	}

	p.mu.Lock()
	var contentCID, packID string
	var expectedSize int64
	if kind == "cas" {
		entry, found := p.cas[digest]
		if !found {
			p.mu.Unlock()
			return object{}, false, nil
		}
		contentCID, packID, expectedSize = entry.CID, entry.PackID, entry.Size
	} else {
		if _, conflict := p.actionConflicts[digest]; conflict {
			p.server.stats.actionDigestConflicts.Add(1)
			p.mu.Unlock()
			return object{}, false, nil
		}
		entry, found := p.actions[digest]
		if !found {
			p.mu.Unlock()
			return object{}, false, nil
		}
		contentCID, packID, expectedSize = entry.CID, entry.PackID, entry.Size
	}
	p.mu.Unlock()

	path, err := p.loadPack(ctx, packID)
	if err != nil {
		return object{}, false, err
	}
	data, err := readCARBlock(ctx, path, contentCID)
	if err != nil {
		return object{}, false, err
	}
	if int64(len(data)) != expectedSize {
		return object{}, false, fmt.Errorf("packed %s/%s has size %d; manifest declares %d", kind, digest, len(data), expectedSize)
	}
	if kind == "cas" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != digest {
			return object{}, false, fmt.Errorf("packed CAS integrity check failed for %s", digest)
		}
	}
	value, err = p.materialize(key, data)
	if err != nil {
		return object{}, false, err
	}
	return value, true, nil
}

func (p *packStore) exists(ctx context.Context, digest string) (bool, error) {
	_, found, err := p.resolve(ctx, p.keyPrefix+"-cas-"+digest, "cas", digest)
	return found, err
}

func (p *packStore) materialize(key string, data []byte) (object, error) {
	p.server.objectsMu.RLock()
	existing, exists := p.server.objects[key]
	p.server.objectsMu.RUnlock()
	if exists {
		return existing, nil
	}
	file, err := os.CreateTemp(p.server.cfg.CacheDir, "packed-object-*")
	if err != nil {
		return object{}, err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return object{}, err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return object{}, err
	}
	value := object{path: path, size: int64(len(data))}
	p.server.objectsMu.Lock()
	if existing, exists := p.server.objects[key]; exists {
		p.server.objectsMu.Unlock()
		return existing, nil
	}
	p.server.objects[key] = value
	p.server.objectsMu.Unlock()
	return value, nil
}

func (p *packStore) loadPack(ctx context.Context, packID string) (string, error) {
	p.mu.Lock()
	if path, exists := p.loadedPacks[packID]; exists {
		p.mu.Unlock()
		return path, nil
	}
	if loading, exists := p.loadingPacks[packID]; exists {
		done := loading.done
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return loading.path, loading.err
		}
	}
	descriptor, exists := p.packs[packID]
	if !exists {
		p.mu.Unlock()
		return "", fmt.Errorf("manifest references unavailable pack %s", packID)
	}
	loading := &packLoad{done: make(chan struct{})}
	p.loadingPacks[packID] = loading
	p.mu.Unlock()

	path, err := p.downloadPack(ctx, descriptor)
	p.mu.Lock()
	if err == nil {
		p.loadedPacks[packID] = path
		loading.path = path
	}
	loading.err = err
	delete(p.loadingPacks, packID)
	close(loading.done)
	p.mu.Unlock()
	return path, err
}

func (p *packStore) downloadPack(ctx context.Context, descriptor packDescriptor) (string, error) {
	if err := p.server.acquire(ctx); err != nil {
		return "", err
	}
	defer p.server.release()
	file, err := os.CreateTemp(p.server.cfg.CacheDir, "pack-download-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	limited := &maxWriter{writer: file, remaining: p.server.cfg.MaxBlobSize}
	backendCtx, cancel := context.WithTimeout(ctx, p.server.cfg.BackendTimeout)
	found, err := p.server.cfg.Backend.Load(backendCtx, descriptor.Key, limited)
	cancel()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("pack %s is missing", descriptor.ID)
	}
	p.server.stats.backendDownloads.Add(1)
	p.server.stats.packDownloads.Add(1)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() != descriptor.Size {
		return "", fmt.Errorf("pack %s has size %d; manifest declares %d", descriptor.ID, info.Size(), descriptor.Size)
	}
	verify, err := os.Open(path)
	if err != nil {
		return "", err
	}
	actual, hashErr := hashFile(verify)
	closeErr := verify.Close()
	if hashErr != nil {
		return "", hashErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if actual != descriptor.ID {
		return "", fmt.Errorf("pack integrity check failed: expected %s, got %s", descriptor.ID, actual)
	}
	return path, nil
}

func (p *packStore) flush(ctx context.Context, all bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		flushed, err := p.flushOneLocked(ctx)
		if err != nil || !flushed || !all {
			return err
		}
	}
}

func (p *packStore) flushOneLocked(ctx context.Context) (bool, error) {
	casDigests := p.selectCASLocked()
	selectedCAS := make(map[string]struct{}, len(casDigests))
	for _, digest := range casDigests {
		selectedCAS[digest] = struct{}{}
	}
	actionDigests := p.selectActionsLocked(selectedCAS)
	if len(casDigests) == 0 && len(actionDigests) == 0 {
		return false, nil
	}

	packPath, entries, packSize, packID, err := p.buildPackLocked(ctx, casDigests, actionDigests)
	if err != nil {
		return false, err
	}
	pack := packDescriptor{ID: packID, Key: packKeyFor(p.keyPrefix, packID), Size: packSize}
	if err := p.publishFile(ctx, pack.Key, packPath, packSize); err != nil {
		return false, fmt.Errorf("publish CARv2 pack: %w", err)
	}
	p.server.stats.packUploads.Add(1)

	manifestValue := p.manifestForPackLocked(pack, entries, selectedCAS)
	manifestData, manifestID, err := encodeManifest(manifestValue)
	if err != nil {
		return false, err
	}
	manifestPath, err := writePrivateTemp(p.server.cfg.CacheDir, "manifest-*", manifestData)
	if err != nil {
		return false, err
	}
	if err := p.publishFile(ctx, manifestKeyFor(p.keyPrefix, manifestID), manifestPath, int64(len(manifestData))); err != nil {
		return false, fmt.Errorf("publish manifest commit point: %w", err)
	}
	p.server.stats.manifestUploads.Add(1)
	p.applyManifestLocked(manifestID, manifestValue)
	p.removePendingLocked(casDigests, actionDigests)
	return true, nil
}

type packedEntries struct {
	cas     []manifestObject
	actions []manifestAction
}

func (p *packStore) buildPackLocked(ctx context.Context, casDigests, actionDigests []string) (string, packedEntries, int64, string, error) {
	type blockEntry struct {
		cid  cid.Cid
		data []byte
	}
	blocksToWrite := make([]blockEntry, 0, len(casDigests)+len(actionDigests))
	entries := packedEntries{}
	for _, digest := range casDigests {
		value := p.pendingCAS[digest]
		data, err := os.ReadFile(value.path)
		if err != nil {
			return "", packedEntries{}, 0, "", err
		}
		contentCID, err := rawCIDForDigest(digest)
		if err != nil {
			return "", packedEntries{}, 0, "", err
		}
		blocksToWrite = append(blocksToWrite, blockEntry{cid: contentCID, data: data})
		entries.cas = append(entries.cas, manifestObject{Digest: digest, CID: contentCID.String(), Size: value.size})
	}
	for _, digest := range actionDigests {
		value := p.pendingActions[digest]
		data, err := os.ReadFile(value.object.path)
		if err != nil {
			return "", packedEntries{}, 0, "", err
		}
		contentCID, err := cid.Decode(value.cid)
		if err != nil {
			return "", packedEntries{}, 0, "", err
		}
		blocksToWrite = append(blocksToWrite, blockEntry{cid: contentCID, data: data})
		entries.actions = append(entries.actions, manifestAction{Digest: digest, CID: value.cid, Size: value.object.size})
	}
	if len(blocksToWrite) == 0 {
		return "", packedEntries{}, 0, "", errors.New("cannot build an empty CARv2 pack")
	}
	path, err := writePrivateTemp(p.server.cfg.CacheDir, "pack-*", nil)
	if err != nil {
		return "", packedEntries{}, 0, "", err
	}
	store, err := blockstore.OpenReadWrite(path, []cid.Cid{blocksToWrite[0].cid}, blockstore.UseWholeCIDs(true))
	if err != nil {
		return "", packedEntries{}, 0, "", err
	}
	for _, entry := range blocksToWrite {
		block, err := blocks.NewBlockWithCid(entry.data, entry.cid)
		if err != nil {
			store.Discard()
			return "", packedEntries{}, 0, "", err
		}
		if err := store.Put(ctx, block); err != nil {
			store.Discard()
			return "", packedEntries{}, 0, "", err
		}
	}
	if err := store.Finalize(); err != nil {
		return "", packedEntries{}, 0, "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", packedEntries{}, 0, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", packedEntries{}, 0, "", err
	}
	packID, hashErr := hashFile(file)
	closeErr := file.Close()
	if hashErr != nil {
		return "", packedEntries{}, 0, "", hashErr
	}
	if closeErr != nil {
		return "", packedEntries{}, 0, "", closeErr
	}
	return path, entries, info.Size(), packID, nil
}

func (p *packStore) manifestForPackLocked(
	pack packDescriptor,
	entries packedEntries,
	selectedCAS map[string]struct{},
) manifest {
	for index := range entries.cas {
		entries.cas[index].PackID = pack.ID
	}
	for index := range entries.actions {
		entries.actions[index].PackID = pack.ID
		closure := make(map[string]struct{})
		pending := p.pendingActions[entries.actions[index].Digest]
		for _, reference := range pending.closure {
			if isImplicitEmptyDigest(reference) {
				continue
			}
			if _, selected := selectedCAS[reference.hash]; selected {
				closure[pack.ID] = struct{}{}
				continue
			}
			if previous, found := p.cas[reference.hash]; found {
				closure[previous.PackID] = struct{}{}
			}
		}
		entries.actions[index].ClosurePacks = sortedStringsFromSet(closure)
	}
	return manifest{
		Version: manifestFormatVersion,
		Parents: sortedStringsFromSet(p.heads),
		Packs:   []packDescriptor{pack},
		CAS:     entries.cas,
		Actions: entries.actions,
	}
}

func (p *packStore) selectCASLocked() []string {
	selected := make([]string, 0)
	var size int64
	for _, digest := range p.pendingCASOrder {
		value, exists := p.pendingCAS[digest]
		if !exists {
			continue
		}
		if len(selected) > 0 && size+value.size > p.targetSize {
			break
		}
		selected = append(selected, digest)
		size += value.size
		if size >= p.targetSize {
			break
		}
	}
	return selected
}

func (p *packStore) selectActionsLocked(selectedCAS map[string]struct{}) []string {
	selected := make([]string, 0)
	for _, digest := range p.pendingACOrder {
		value, exists := p.pendingActions[digest]
		if !exists {
			continue
		}
		eligible := true
		for _, reference := range value.closure {
			if isImplicitEmptyDigest(reference) {
				continue
			}
			if _, selected := selectedCAS[reference.hash]; selected {
				continue
			}
			if _, published := p.cas[reference.hash]; !published {
				eligible = false
				break
			}
		}
		if eligible {
			selected = append(selected, digest)
		}
	}
	return selected
}

func (p *packStore) publishFile(ctx context.Context, key, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	deduplicated, err := p.server.publishOnce(ctx, key, file, size)
	if err != nil {
		return err
	}
	if deduplicated {
		p.server.stats.deduplicatedUploads.Add(1)
	} else {
		p.server.stats.uploads.Add(1)
	}
	return nil
}

func (p *packStore) removePendingLocked(casDigests, actionDigests []string) {
	for _, digest := range casDigests {
		if value, exists := p.pendingCAS[digest]; exists {
			p.pendingBytes -= value.size
			delete(p.pendingCAS, digest)
		}
	}
	for _, digest := range actionDigests {
		if value, exists := p.pendingActions[digest]; exists {
			p.pendingBytes -= value.object.size
			delete(p.pendingActions, digest)
		}
	}
	p.pendingCASOrder = filterPending(p.pendingCASOrder, p.pendingCAS)
	p.pendingACOrder = filterPending(p.pendingACOrder, p.pendingActions)
}

func filterPending[T any](order []string, values map[string]T) []string {
	filtered := order[:0]
	for _, key := range order {
		if _, exists := values[key]; exists {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

func (p *packStore) discover(ctx context.Context) error {
	keys, err := p.catalog.List(ctx, manifestKeyFor(p.keyPrefix, ""), p.maxManifests)
	if err != nil {
		return err
	}
	p.server.stats.manifestsDiscovered.Add(uint64(len(keys)))
	manifests := make(map[string]manifest, len(keys))
	parents := make(map[string]struct{})
	for _, key := range keys {
		manifestID, ok := parseManifestKey(p.keyPrefix, key)
		if !ok {
			continue
		}
		value, err := p.loadManifest(ctx, key, manifestID)
		if err != nil {
			p.server.stats.manifestLoadErrors.Add(1)
			p.server.cfg.Logger.Printf("ignoring unavailable manifest %s: %s", manifestID, safeError(err))
			continue
		}
		manifests[manifestID] = value
		for _, parent := range value.Parents {
			parents[parent] = struct{}{}
		}
	}
	ids := make([]string, 0, len(manifests))
	for id := range manifests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.applyManifestLocked(id, manifests[id])
		if _, parent := parents[id]; !parent {
			p.heads[id] = struct{}{}
		}
	}
	return nil
}

func (p *packStore) loadManifest(ctx context.Context, key, id string) (manifest, error) {
	if err := p.server.acquire(ctx); err != nil {
		return manifest{}, err
	}
	defer p.server.release()
	var data bytesBuffer
	backendCtx, cancel := context.WithTimeout(ctx, p.server.cfg.BackendTimeout)
	found, err := p.server.cfg.Backend.Load(backendCtx, key, &data)
	cancel()
	if err != nil {
		return manifest{}, err
	}
	if !found {
		return manifest{}, errors.New("manifest cache entry is missing")
	}
	p.server.stats.backendDownloads.Add(1)
	if int64(data.Len()) > p.server.cfg.MaxBlobSize {
		return manifest{}, errors.New("manifest exceeds maximum object size")
	}
	return decodeManifest(data.Bytes(), id)
}

func (p *packStore) applyManifestLocked(id string, value manifest) {
	for _, descriptor := range value.Packs {
		if existing, found := p.packs[descriptor.ID]; !found || descriptor.Key < existing.Key {
			p.packs[descriptor.ID] = descriptor
		}
	}
	for _, object := range value.CAS {
		if existing, found := p.cas[object.Digest]; !found || object.PackID < existing.PackID {
			p.cas[object.Digest] = object
		}
	}
	for _, action := range value.Actions {
		if existing, found := p.actions[action.Digest]; found {
			if existing.CID != action.CID {
				p.actionConflicts[action.Digest] = struct{}{}
			}
			continue
		}
		p.actions[action.Digest] = action
	}
	for _, parent := range value.Parents {
		delete(p.heads, parent)
	}
	p.heads[id] = struct{}{}
}

func parseManifestKey(prefix, key string) (string, bool) {
	head := manifestKeyFor(prefix, "")
	if len(key) <= len(head) || key[:len(head)] != head {
		return "", false
	}
	id := key[len(head):]
	return id, validCID(id)
}

func readCARBlock(ctx context.Context, path, contentCID string) ([]byte, error) {
	store, err := blockstore.OpenReadOnly(path, blockstore.UseWholeCIDs(true))
	if err != nil {
		return nil, fmt.Errorf("open CARv2 pack: %w", err)
	}
	defer store.Close()
	content, err := cid.Decode(contentCID)
	if err != nil {
		return nil, err
	}
	block, err := store.Get(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("load block from CARv2 pack: %w", err)
	}
	data := block.RawData()
	actual, err := rawCIDForData(data)
	if err != nil || actual.String() != contentCID {
		return nil, errors.New("CARv2 block CID integrity check failed")
	}
	return data, nil
}

func writePrivateTemp(directory, pattern string, data []byte) (string, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	if len(data) > 0 {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) Write(data []byte) (int, error) {
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }
func (b *bytesBuffer) Len() int      { return len(b.data) }

func sortDigestReferences(values []digestReference) []digestReference {
	unique := make(map[string]digestReference, len(values))
	for _, value := range values {
		if existing, found := unique[value.hash]; !found || existing.size == value.size {
			unique[value.hash] = value
		}
	}
	sorted := make([]digestReference, 0, len(unique))
	for _, value := range unique {
		sorted = append(sorted, value)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].hash < sorted[j].hash })
	return sorted
}

func sortedStringsFromSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

var _ io.Writer = (*bytesBuffer)(nil)
