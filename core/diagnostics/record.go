// Package diagnostics carries source-owned execution evidence independently of
// application state. Adding an event never requires a consumer-side vocabulary.
package diagnostics

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"
)

// Value admits only detached JSON values. Native objects, callbacks and errors
// cannot escape into a recorder or retain a connection through its queue.
type Value struct{ value any }
type Fields map[string]Value

const MaximumTextBytes = 16 * 1024

func Text(value string) Value {
	if len(value) > MaximumTextBytes {
		value = strings.ToValidUTF8(value[:MaximumTextBytes], "�") + "…"
	}
	return Value{strings.Clone(value)}
}
func Bool(value bool) Value { return Value{value} }

// Nonfinite measurements retain their diagnostic spelling instead of rejecting the record.
func Number(value float64) Value {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Text(strconv.FormatFloat(value, 'g', -1, 64))
	}
	return Value{value}
}

// Counters use decimal strings so JSON consumers retain all 64 bits.
func Uint(value uint64) Value          { return Text(strconv.FormatUint(value, 10)) }
func Int(value int64) Value            { return Text(strconv.FormatInt(value, 10)) }
func Millis(value time.Duration) Value { return Int(value.Milliseconds()) }
func Identity(value []byte) Value      { return Text(base64.RawURLEncoding.EncodeToString(value)) }
func Hex(value []byte) Value           { return Text(hex.EncodeToString(value)) }
func Object(fields Fields) Value {
	copy := make(Fields, len(fields))
	maps.Copy(copy, fields)
	return Value{copy}
}
func Array(values ...Value) Value                { return Value{append([]Value{}, values...)} }
func (value Value) MarshalJSON() ([]byte, error) { return json.Marshal(value.value) }
func (value Value) Text() string {
	if number, ok := value.value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	text, _ := value.value.(string)
	return text
}
func (value Value) Bool() bool   { flag, _ := value.value.(bool); return flag }
func (value Value) Uint() uint64 { number, _ := strconv.ParseUint(value.Text(), 10, 64); return number }
func (value Value) Get(key string) Value {
	fields, _ := value.value.(Fields)
	return fields[key]
}

type Scope struct {
	EngineID string `json:"engine_id,omitempty"`
	TaskID   string `json:"engine_task_id,omitempty"`
	Workflow string `json:"workflow,omitempty"`
}

// Record is immutable after construction. A sink adds ownership without
// interpreting the event's payload or replacing its source timestamp.
type Record struct {
	name        string
	at          time.Time
	scope       Scope
	fields      Value
	correlation Value
}

func New(name string, fields Fields) Record  { return Record{name: name, fields: Object(fields)} }
func (record Record) At(at time.Time) Record { record.at = at; return record }
func (record Record) Correlate(fields Fields) Record {
	combined := Fields{}
	if existing, ok := record.correlation.value.(Fields); ok {
		maps.Copy(combined, existing)
	}
	maps.Copy(combined, fields)
	record.correlation = Object(combined)
	return record
}
func (record Record) Name() string          { return record.name }
func (record Record) ObservedAt() time.Time { return record.at }
func (record Record) Scope() Scope          { return record.scope }
func (record Record) Payload() Value        { return record.fields }
func (record Record) Correlation() Value    { return record.correlation }
func (record Record) Valid() bool           { return record.name != "" && record.fields.value != nil }

// With adds owning-context facts to a detached copy.
func (record Record) With(fields Fields) Record {
	combined := Fields{}
	if existing, ok := record.fields.value.(Fields); ok {
		maps.Copy(combined, existing)
	}
	maps.Copy(combined, fields)
	record.fields = Object(combined)
	return record
}

func (value Value) Present() bool { return value.value != nil }
