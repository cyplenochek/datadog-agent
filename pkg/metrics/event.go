// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"

	"github.com/gogo/protobuf/proto"
	jsoniter "github.com/json-iterator/go"

	agentpayload "github.com/DataDog/agent-payload/gogen"
	"github.com/DataDog/datadog-agent/pkg/serializer/marshaler"
	"github.com/DataDog/datadog-agent/pkg/util"
	"github.com/DataDog/datadog-agent/pkg/util/jsonrawobjectwriter"
)

// EventPriority represents the priority of an event
type EventPriority string

// Enumeration of the existing event priorities, and their values
const (
	EventPriorityNormal       EventPriority = "normal"
	EventPriorityLow          EventPriority = "low"
	apiKeyJSONField                         = "apiKey"
	eventsJSONField                         = "events"
	internalHostnameJSONField               = "internalHostname"
	outOfRangeMsg                           = "out of range"
)

var eventExpvar = expvar.NewMap("Event")

// GetEventPriorityFromString returns the EventPriority from its string representation
func GetEventPriorityFromString(val string) (EventPriority, error) {
	switch val {
	case string(EventPriorityNormal):
		return EventPriorityNormal, nil
	case string(EventPriorityLow):
		return EventPriorityLow, nil
	default:
		return "", fmt.Errorf("Invalid event priority: '%s'", val)
	}
}

// EventAlertType represents the alert type of an event
type EventAlertType string

// Enumeration of the existing event alert types, and their values
const (
	EventAlertTypeError   EventAlertType = "error"
	EventAlertTypeWarning EventAlertType = "warning"
	EventAlertTypeInfo    EventAlertType = "info"
	EventAlertTypeSuccess EventAlertType = "success"
)

// GetAlertTypeFromString returns the EventAlertType from its string representation
func GetAlertTypeFromString(val string) (EventAlertType, error) {
	switch val {
	case string(EventAlertTypeError):
		return EventAlertTypeError, nil
	case string(EventAlertTypeWarning):
		return EventAlertTypeWarning, nil
	case string(EventAlertTypeInfo):
		return EventAlertTypeInfo, nil
	case string(EventAlertTypeSuccess):
		return EventAlertTypeSuccess, nil
	default:
		return EventAlertTypeInfo, fmt.Errorf("Invalid alert type: '%s'", val)
	}
}

// Event holds an event (w/ serialization to DD agent 5 intake format)
type Event struct {
	Title          string         `json:"msg_title"`
	Text           string         `json:"msg_text"`
	Ts             int64          `json:"timestamp"`
	Priority       EventPriority  `json:"priority,omitempty"`
	Host           string         `json:"host"`
	Tags           []string       `json:"tags,omitempty"`
	AlertType      EventAlertType `json:"alert_type,omitempty"`
	AggregationKey string         `json:"aggregation_key,omitempty"`
	SourceTypeName string         `json:"source_type_name,omitempty"`
	EventType      string         `json:"event_type,omitempty"`
}

// Return a JSON string or "" in case of error during the Marshaling
func (e *Event) String() string {
	s, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	return string(s)
}

// Events represents a list of events ready to be serialize
type Events []*Event

// Marshal serialize events using agent-payload definition
func (events Events) Marshal() ([]byte, error) {
	payload := &agentpayload.EventsPayload{
		Events:   []*agentpayload.EventsPayload_Event{},
		Metadata: &agentpayload.CommonMetadata{},
	}

	for _, e := range events {
		payload.Events = append(payload.Events,
			&agentpayload.EventsPayload_Event{
				Title:          e.Title,
				Text:           e.Text,
				Ts:             e.Ts,
				Priority:       string(e.Priority),
				Host:           e.Host,
				Tags:           e.Tags,
				AlertType:      string(e.AlertType),
				AggregationKey: e.AggregationKey,
				SourceTypeName: e.SourceTypeName,
			})
	}

	return proto.Marshal(payload)
}

func (events Events) getEventsBySourceType() map[string][]*Event {
	eventsBySourceType := make(map[string][]*Event)
	for _, e := range events {
		sourceTypeName := e.SourceTypeName
		if sourceTypeName == "" {
			sourceTypeName = "api"
		}

		eventsBySourceType[sourceTypeName] = append(eventsBySourceType[sourceTypeName], e)
	}
	return eventsBySourceType
}

// MarshalJSON serializes events to JSON so it can be sent to the Agent 5 intake
// (we don't use the v1 event endpoint because it only supports 1 event per payload)
//FIXME(olivier): to be removed when v2 endpoints are available
func (events Events) MarshalJSON() ([]byte, error) {
	// Regroup events by their source type name
	eventsBySourceType := events.getEventsBySourceType()
	hostname, _ := util.GetHostname()
	// Build intake payload containing events and serialize
	data := map[string]interface{}{
		apiKeyJSONField:           "", // legacy field, it isn't actually used by the backend
		eventsJSONField:           eventsBySourceType,
		internalHostnameJSONField: hostname,
	}
	reqBody := &bytes.Buffer{}
	err := json.NewEncoder(reqBody).Encode(data)
	return reqBody.Bytes(), err
}

// SplitPayload breaks the payload into times number of pieces
func (events Events) SplitPayload(times int) ([]marshaler.Marshaler, error) {
	eventExpvar.Add("TimesSplit", 1)
	// An individual event cannot be split,
	// we can only split up the events

	// only split as much as possible
	if len(events) < times {
		eventExpvar.Add("EventsShorter", 1)
		times = len(events)
	}
	splitPayloads := make([]marshaler.Marshaler, times)

	batchSize := len(events) / times
	n := 0
	for i := 0; i < times; i++ {
		var end int
		// the batchSize won't be perfect, in most cases there will be more or less in the last one than the others
		if i < times-1 {
			end = n + batchSize
		} else {
			end = len(events)
		}
		newEvents := Events(events[n:end])
		splitPayloads[i] = newEvents
		n += batchSize
	}
	return splitPayloads, nil
}

//-----------------------------------------------------------------------------
// Implements StreamJSONMarshaler.
// Each item in StreamJSONMarshaler is all events for a specific source type name.
//-----------------------------------------------------------------------------
type eventsSourceType struct {
	sourceType string
	events     []*Event
}

type eventsBySourceTypeMarshaler struct {
	Events             // Require to not implement Marshaler methods
	eventsBySourceType []eventsSourceType
}

func (*eventsBySourceTypeMarshaler) WriteHeader(stream *jsoniter.Stream) error {
	writeEventsHeader(stream)
	return stream.Flush()
}

func writeEventsHeader(stream *jsoniter.Stream) {
	stream.WriteObjectStart()
	stream.WriteObjectField(apiKeyJSONField)
	stream.WriteString("")
	stream.WriteMore()

	stream.WriteObjectField(eventsJSONField)
	stream.WriteObjectStart()
}

func (*eventsBySourceTypeMarshaler) WriteFooter(stream *jsoniter.Stream) error {
	return writeEventsFooter(stream)
}

func writeEventsFooter(stream *jsoniter.Stream) error {
	stream.WriteObjectEnd()
	stream.WriteMore()

	hostname, _ := util.GetHostname()
	stream.WriteObjectField(internalHostnameJSONField)
	stream.WriteString(hostname)

	stream.WriteObjectEnd()
	return stream.Flush()
}

func (e *eventsBySourceTypeMarshaler) WriteItem(stream *jsoniter.Stream, i int) error {
	if i < 0 || i > len(e.eventsBySourceType)-1 {
		return errors.New(outOfRangeMsg)
	}

	writer := jsonrawobjectwriter.NewJSONRawObjectWriter(stream)
	eventSourceType := e.eventsBySourceType[i]
	writer.StartArrayField(eventSourceType.sourceType)
	for _, v := range eventSourceType.events {
		if err := writeEvent(v, writer); err != nil {
			return err
		}
	}
	writer.FinishArrayField()
	return nil
}

func (e *eventsBySourceTypeMarshaler) Len() int { return len(e.eventsBySourceType) }

func (e *eventsBySourceTypeMarshaler) DescribeItem(i int) string {
	if i < 0 || i > len(e.eventsBySourceType)-1 {
		return outOfRangeMsg
	}
	return fmt.Sprintf("Source type: %s, events count: %d", e.eventsBySourceType[i].sourceType, len(e.eventsBySourceType[i].events))
}

func writeEvent(event *Event, writer *jsonrawobjectwriter.JSONRawObjectWriter) error {
	writer.StartObject()
	writer.AddStringField("msg_title", event.Title, jsonrawobjectwriter.AllowEmpty)
	writer.AddStringField("msg_text", event.Text, jsonrawobjectwriter.AllowEmpty)
	writer.AddInt64Field("timestamp", event.Ts)
	writer.AddStringField("priority", string(event.Priority), jsonrawobjectwriter.OmitEmpty)
	writer.AddStringField("host", event.Host, jsonrawobjectwriter.AllowEmpty)

	if len(event.Tags) != 0 {
		writer.StartArrayField("tags")
		for _, tag := range event.Tags {
			writer.AddStringValue(tag)
		}
		writer.FinishArrayField()
	}

	writer.AddStringField("alert_type", string(event.AlertType), jsonrawobjectwriter.OmitEmpty)
	writer.AddStringField("aggregation_key", event.AggregationKey, jsonrawobjectwriter.OmitEmpty)
	writer.AddStringField("source_type_name", event.SourceTypeName, jsonrawobjectwriter.OmitEmpty)
	writer.AddStringField("event_type", event.EventType, jsonrawobjectwriter.OmitEmpty)
	writer.FinishObject()
	return writer.Flush()
}

// CreateMarshalerBySourceType creates marshaler.StreamJSONMarshaler where each item in StreamJSONMarshaler is all events for a specific source type name.
func (events Events) CreateMarshalerBySourceType() marshaler.StreamJSONMarshaler {
	eventsBySourceType := events.getEventsBySourceType()
	var values []eventsSourceType
	for sourceType, events := range eventsBySourceType {
		values = append(values, eventsSourceType{sourceType, events})
	}
	return &eventsBySourceTypeMarshaler{events, values}
}

//-----------------------------------------------------------------------------
// Implements a collection of StreamJSONMarshaler.
// Each StreamJSONMarshaler is all events for a specific source type name.
//-----------------------------------------------------------------------------
type eventsMarshaler struct {
	sourceTypeName string
	Events
}

func (e *eventsMarshaler) WriteHeader(stream *jsoniter.Stream) error {
	writeEventsHeader(stream)
	stream.WriteObjectField(e.sourceTypeName)
	stream.WriteArrayStart()
	return stream.Flush()
}

func (e *eventsMarshaler) WriteFooter(stream *jsoniter.Stream) error {
	stream.WriteArrayEnd()
	return writeEventsFooter(stream)
}

func (e *eventsMarshaler) WriteItem(stream *jsoniter.Stream, i int) error {
	if i < 0 || i > len(e.Events)-1 {
		return errors.New(outOfRangeMsg)
	}

	event := e.Events[i]
	writer := jsonrawobjectwriter.NewJSONRawObjectWriter(stream)
	if err := writeEvent(event, writer); err != nil {
		return err
	}

	return writer.Flush()
}

func (e *eventsMarshaler) Len() int { return len(e.Events) }

func (e *eventsMarshaler) DescribeItem(i int) string {
	if i < 0 || i > len(e.Events)-1 {
		return outOfRangeMsg
	}
	event := e.Events[i]
	return fmt.Sprintf("Title: %s, Text: %s, Source Type: %s", event.Title, event.Text, event.SourceTypeName)
}

// CreateMarshalerForEachSourceType creates a collection of marshaler.StreamJSONMarshaler.
// Each StreamJSONMarshaler is all events for a specific source type name.
func (events Events) CreateMarshalerForEachSourceType() []marshaler.StreamJSONMarshaler {
	e := events.getEventsBySourceType()
	var values []marshaler.StreamJSONMarshaler
	for k, v := range e {
		values = append(values, &eventsMarshaler{k, v})
	}

	// Make sure we return at least one marshaler
	if len(values) == 0 {
		values = append(values, &eventsBySourceTypeMarshaler{events, nil})
	}
	return values
}
