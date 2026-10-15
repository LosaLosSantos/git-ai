package log

import (
	"context"
	"sync"
)

type requestCustomFieldsKey struct{}

// CustomFields stores custom fields, which will be logged as a part of gRPC logs. The gRPC server is expected to add
// corresponding interceptors. They initialize a CustomFields object and inject it into the context. Callers can pull
// the object out with CustomFieldsFromContext.
type CustomFields struct {
	numericFields map[string]int
	anyFields     map[string]any
	sync.Mutex
}

// RecordSum sums up all the values for a given key.
func (fields *CustomFields) RecordSum(key string, value int) {
	fields.Lock()
	defer fields.Unlock()

	if prevValue, ok := fields.numericFields[key]; ok {
		value += prevValue
	}

	fields.numericFields[key] = value
}

// RecordMax will store the max value for a given key.
func (fields *CustomFields) RecordMax(key string, value int) {
	fields.Lock()
	defer fields.Unlock()

	if prevValue, ok := fields.numericFields[key]; ok {
		if prevValue > value {
			return
		}
	}

	fields.numericFields[key] = value
}

// RecordMetadata records a string metadata for the given key.
func (fields *CustomFields) RecordMetadata(key string, value any) {
	fields.Lock()
	defer fields.Unlock()

	fields.anyFields[key] = value
}

// Fields returns all the fields as Fields
func (fields *CustomFields) Fields() Fields {
	fields.Lock()
	defer fields.Unlock()

	f := Fields{}
	for k, v := range fields.numericFields {
		f[k] = v
	}
	for k, v := range fields.anyFields {
		f[k] = v
	}
	return f
}

// CustomFieldsFromContext gets the `CustomFields` from the given context.
func CustomFieldsFromContext(ctx context.Context) *CustomFields {
	fields, _ := ctx.Value(requestCustomFieldsKey{}).(*CustomFields)
	return fields
}

// InitContextCustomFields returns a new context with `CustomFields` added to the given context.
func InitContextCustomFields(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestCustomFieldsKey{}, &CustomFields{
		numericFields: make(map[string]int),
		anyFields:     make(map[string]any),
	})
}
