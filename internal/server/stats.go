package server

import (
	"encoding/json"
	"sync/atomic"
)

// Stats is a point-in-time JSON representation of cache activity.
type Stats struct {
	Requests                   uint64 `json:"requests"`
	Hits                       uint64 `json:"hits"`
	Misses                     uint64 `json:"misses"`
	Uploads                    uint64 `json:"uploads"`
	DiscardedUploads           uint64 `json:"discarded_uploads"`
	BackendDownloads           uint64 `json:"backend_downloads"`
	BackendExistenceChecks     uint64 `json:"backend_existence_checks"`
	BackendLoadErrors          uint64 `json:"backend_load_errors"`
	BackendSaveErrors          uint64 `json:"backend_save_errors"`
	RejectedRequests           uint64 `json:"rejected_requests"`
	ValidatedActionResults     uint64 `json:"validated_action_results"`
	IncompleteActionResults    uint64 `json:"incomplete_action_results"`
	InvalidActionResults       uint64 `json:"invalid_action_results"`
	SkippedActionResultUploads uint64 `json:"skipped_action_result_uploads"`
	BytesServed                uint64 `json:"bytes_served"`
	BytesReceived              uint64 `json:"bytes_received"`
	ThrottleWaits              uint64 `json:"throttle_waits"`
}

type counters struct {
	requests                   atomic.Uint64
	hits                       atomic.Uint64
	misses                     atomic.Uint64
	uploads                    atomic.Uint64
	discardedUploads           atomic.Uint64
	backendDownloads           atomic.Uint64
	backendExistenceChecks     atomic.Uint64
	backendLoadErrors          atomic.Uint64
	backendSaveErrors          atomic.Uint64
	rejectedRequests           atomic.Uint64
	validatedActionResults     atomic.Uint64
	incompleteActionResults    atomic.Uint64
	invalidActionResults       atomic.Uint64
	skippedActionResultUploads atomic.Uint64
	bytesServed                atomic.Uint64
	bytesReceived              atomic.Uint64
	throttleWaits              atomic.Uint64
}

func (c *counters) snapshot() Stats {
	return Stats{
		Requests:                   c.requests.Load(),
		Hits:                       c.hits.Load(),
		Misses:                     c.misses.Load(),
		Uploads:                    c.uploads.Load(),
		DiscardedUploads:           c.discardedUploads.Load(),
		BackendDownloads:           c.backendDownloads.Load(),
		BackendExistenceChecks:     c.backendExistenceChecks.Load(),
		BackendLoadErrors:          c.backendLoadErrors.Load(),
		BackendSaveErrors:          c.backendSaveErrors.Load(),
		RejectedRequests:           c.rejectedRequests.Load(),
		ValidatedActionResults:     c.validatedActionResults.Load(),
		IncompleteActionResults:    c.incompleteActionResults.Load(),
		InvalidActionResults:       c.invalidActionResults.Load(),
		SkippedActionResultUploads: c.skippedActionResultUploads.Load(),
		BytesServed:                c.bytesServed.Load(),
		BytesReceived:              c.bytesReceived.Load(),
		ThrottleWaits:              c.throttleWaits.Load(),
	}
}

func (s Stats) JSON() []byte {
	data, _ := json.Marshal(s)
	return data
}
