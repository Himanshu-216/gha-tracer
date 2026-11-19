// Package ids derives deterministic trace and span IDs from GitHub identifiers,
// so reprocessing the same run (redelivery, retries, restarts) emits identical
// spans that Tempo deduplicates.
package ids

import (
	"crypto/sha256"
	"fmt"
	"strconv"
)

type TraceID [16]byte
type SpanID [8]byte

// RunKey is the canonical identity of one run attempt: "org/repo#runID#attempt".
func RunKey(repo string, runID int64, attempt int) string {
	return fmt.Sprintf("%s#%d#%d", repo, runID, attempt)
}

func Trace(runKey string) TraceID {
	var t TraceID
	h := sha256.Sum256([]byte("run:" + runKey))
	copy(t[:], h[:16])
	return t
}

func RunSpan(runKey string) SpanID { return span("runspan:" + runKey) }

func CallSpan(runKey, callerJob string) SpanID {
	return span("call:" + runKey + ":" + callerJob)
}

func JobSpan(jobID int64) SpanID { return span("job:" + strconv.FormatInt(jobID, 10)) }

func StepSpan(jobID int64, step int) SpanID {
	return span("step:" + strconv.FormatInt(jobID, 10) + ":" + strconv.Itoa(step))
}

func span(s string) SpanID {
	var id SpanID
	h := sha256.Sum256([]byte(s))
	copy(id[:], h[:8])
	return id
}
