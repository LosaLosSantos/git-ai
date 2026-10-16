package sdk

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// objectBatchSize is the initial number of object IDs per GetObjects
	// request. It stays below the server default of ObjectService.MaxObjects.
	// A server configured with a lower limit rejects larger requests, and the
	// client then lowers its batch size to that limit (see objectLimit).
	objectBatchSize = 500
	// maxObjectFailures is the number of consecutive failed attempts without
	// progress after which GetObjects gives up.
	maxObjectFailures = 5
	// objectRetryBackoff is the base delay between resume attempts.
	objectRetryBackoff = 100 * time.Millisecond
)

// Object is one object returned by GetObjects.
type Object struct {
	// OID is the object ID that was requested.
	OID string
	// Type is the object type. Zero if the object is missing.
	Type ObjectType
	// Size is the total size of the object in bytes.
	Size int64
	// Data is the raw object content, complete for non-missing objects.
	Data []byte
	// Missing is true if the object does not exist in the repository.
	Missing bool
}

// GetObjects fetches the raw contents of the given objects.
//
// The result has one entry per distinct ID, in the order of first occurrence
// in oids. Objects that do not exist are returned with Missing set; they do
// not cause an error. Requests are split into batches, and interrupted
// transfers are resumed automatically from the last received byte. When the
// server allows fewer object IDs per request than a batch holds, the batch is
// split further, and the client keeps the smaller size for later calls.
//
// All object contents are held in memory. Callers that fetch very large
// objects should fetch them in smaller sets.
func (r *Repo) GetObjects(ctx context.Context, oids []string) ([]Object, error) {
	unique, err := uniqueOIDs(oids)
	if err != nil {
		return nil, err
	}

	result := make([]Object, 0, len(unique))
	for start := 0; start < len(unique); {
		end := min(start+r.client.objectBatchSize(), len(unique))

		batch, err := r.getObjectBatch(ctx, unique[start:end])
		if limit, ok := objectLimit(err, end-start); ok {
			r.client.lowerObjectBatchSize(limit)
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, batch...)
		start = end
	}

	return result, nil
}

// objectLimit reports whether err is the server's refusal of a request for
// sent object IDs because it allows fewer per request (max_objects), and how
// many IDs to send instead. The server names its maximum in the error message;
// without it, the batch is halved. A single ID is never refused this way.
func objectLimit(err error, sent int) (int, bool) {
	if sent <= 1 || status.Code(err) != codes.InvalidArgument {
		return 0, false
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "too many object IDs") {
		return 0, false
	}
	if _, after, found := strings.Cut(msg, "maximum is "); found {
		end := 0
		for end < len(after) && after[end] >= '0' && after[end] <= '9' {
			end++
		}
		if limit, err := strconv.Atoi(after[:end]); err == nil && limit >= 1 && limit < sent {
			return limit, true
		}
	}
	return sent / 2, true
}

// uniqueOIDs removes duplicate object IDs, keeping the first occurrence.
func uniqueOIDs(oids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(oids))
	unique := make([]string, 0, len(oids))
	for _, oid := range oids {
		if oid == "" {
			return nil, status.Error(codes.InvalidArgument, "empty object ID")
		}
		if _, ok := seen[oid]; ok {
			continue
		}
		seen[oid] = struct{}{}
		unique = append(unique, oid)
	}

	return unique, nil
}

// getObjectBatch fetches one batch of distinct object IDs, resuming the
// transfer after transient failures.
func (r *Repo) getObjectBatch(ctx context.Context, oids []string) ([]Object, error) {
	rc := &objectReceiver{expected: oids}
	failures := 0

	for {
		rc.flush()
		if rc.complete() {
			return rc.done, nil
		}

		req := &gitalypb.GetObjectsRequest{
			Repository: r.repository(),
			Oids:       oids[len(rc.done):],
		}
		if rc.partial != nil {
			req.ResumeOid = rc.partial.OID
			req.ResumeOffset = int64(len(rc.partial.Data))
		}

		doneBefore, receivedBefore := len(rc.done), rc.received
		err := r.receiveObjects(ctx, req, rc)

		rc.flush()
		if rc.complete() {
			return rc.done, nil
		}
		if err == nil {
			err = status.Error(codes.Internal, "GetObjects stream ended before all objects were received")
		}
		if ctx.Err() != nil || !isTransient(err) {
			return nil, err
		}

		if len(rc.done) > doneBefore || rc.received > receivedBefore {
			failures = 0
		} else {
			failures++
		}
		if failures >= maxObjectFailures {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(objectRetryBackoff * time.Duration(failures+1)):
		}
	}
}

// receiveObjects runs one GetObjects stream and feeds its messages to rc.
func (r *Repo) receiveObjects(ctx context.Context, req *gitalypb.GetObjectsRequest, rc *objectReceiver) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.client.objects.GetObjects(ctx, req)
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		if err := rc.handle(msg); err != nil {
			return err
		}
	}
}

// objectReceiver reassembles objects from GetObjects messages. It keeps the
// object that is still being received across attempts, so a resumed request
// continues where the previous stream stopped.
type objectReceiver struct {
	// expected is the batch of object IDs, in request order.
	expected []string
	// done holds the completed objects, in request order.
	done []Object
	// partial is the object being received, if any.
	partial *Object
	// received counts the data bytes received over all attempts.
	received int64
}

func (rc *objectReceiver) complete() bool {
	return len(rc.done) == len(rc.expected)
}

// flush moves the object in progress to done once it is complete.
func (rc *objectReceiver) flush() {
	if rc.partial != nil && rc.partial.isComplete() {
		rc.done = append(rc.done, *rc.partial)
		rc.partial = nil
	}
}

func (rc *objectReceiver) handle(msg *gitalypb.GetObjectsResponse) error {
	if rc.partial != nil && rc.partial.OID != msg.GetOid() {
		rc.flush()
		if rc.partial != nil {
			return status.Errorf(codes.Internal, "object %q is incomplete", rc.partial.OID)
		}
	}

	if rc.partial == nil {
		if rc.complete() || rc.expected[len(rc.done)] != msg.GetOid() {
			return status.Errorf(codes.Internal, "unexpected object %q in GetObjects response", msg.GetOid())
		}
		rc.partial = &Object{OID: msg.GetOid()}
	}

	obj := rc.partial
	if msg.GetMissing() {
		obj.Missing = true
		return nil
	}

	if msg.GetType() != gitalypb.ObjectType_UNKNOWN {
		obj.Type = msg.GetType()
	}
	if msg.GetSize() != 0 {
		obj.Size = msg.GetSize()
	}

	if msg.GetOffset() != int64(len(obj.Data)) {
		return status.Errorf(codes.Internal, "object %q: chunk at offset %d, expected %d",
			obj.OID, msg.GetOffset(), len(obj.Data))
	}

	obj.Data = append(obj.Data, msg.GetData()...)
	rc.received += int64(len(msg.GetData()))

	return nil
}

// isComplete reports whether the object was fully received. The type is only
// known once the first message of the object arrived, so an object without
// it is never complete.
func (o *Object) isComplete() bool {
	if o.Missing {
		return true
	}
	return o.Type != gitalypb.ObjectType_UNKNOWN && int64(len(o.Data)) == o.Size
}

// isTransient reports whether an error may go away when the request is
// retried, such as a dropped connection.
func isTransient(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Internal, codes.Unknown:
		return true
	default:
		return false
	}
}
