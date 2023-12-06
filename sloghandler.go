//go:build go1.21
// +build go1.21

/*
Copyright 2023 The logr Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logr

import (
	"context"
	"log/slog"
)

type slogHandler struct {
	// May be nil, in which case all logs get discarded.
	sink LogSink
	// Non-nil if sink is non-nil and implements SlogSink.
	slogSink SlogSink

	// groupName is the name of the current group.
	groupName string

	// these keep track of the current set of values, the direct parent of this
	// set, and the root of the tree.
	currentValues []any
	parentValues  map[string]any
	rootValues    map[string]any

	// levelBias can be set when constructing the handler to influence the
	// slog.Level of log records. A positive levelBias reduces the
	// slog.Level value. slog has no API to influence this value after the
	// handler got created, so it can only be set indirectly through
	// Logger.V.
	levelBias slog.Level
}

var _ slog.Handler = &slogHandler{}

// GetLevel is used for black box unit testing.
func (l *slogHandler) GetLevel() slog.Level {
	return l.levelBias
}

func (l *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return l.sink != nil && (level >= slog.LevelError || l.sink.Enabled(l.levelFromSlog(level)))
}

func (l *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	if l.slogSink != nil {
		// Only adjust verbosity level of log entries < slog.LevelError.
		if record.Level < slog.LevelError {
			record.Level -= l.levelBias
		}
		return l.slogSink.Handle(ctx, record)
	}

	// No need to check for nil sink here because Handle will only be called
	// when Enabled returned true.

	var kvList []any

	// Collect all the values for this group.
	if n := len(l.currentValues) + record.NumAttrs(); n > 0 {
		kvList = make([]any, 0, 2*n)
		kvList = append(kvList, l.currentValues...)
		record.Attrs(func(attr slog.Attr) bool {
			kvList = attrToKVs(attr, kvList)
			return true
		})
		if l.parentValues != nil {
			l.parentValues[l.groupName] = listToMap(kvList)
		}
	}

	// If this is under group, start at the root of the group structure.
	if l.parentValues != nil {
		kvList = mapToList(l.rootValues)
	}

	if record.Level >= slog.LevelError {
		l.sinkWithCallDepth().Error(nil, record.Message, kvList...)
	} else {
		level := l.levelFromSlog(record.Level)
		l.sinkWithCallDepth().Info(level, record.Message, kvList...)
	}
	return nil
}

// sinkWithCallDepth adjusts the stack unwinding so that when Error or Info
// are called by Handle, code in slog gets skipped.
//
// This offset currently (Go 1.21.0) works for calls through
// slog.New(ToSlogHandler(...)).  There's no guarantee that the call
// chain won't change. Wrapping the handler will also break unwinding. It's
// still better than not adjusting at all....
//
// This cannot be done when constructing the handler because FromSlogHandler needs
// access to the original sink without this adjustment. A second copy would
// work, but then WithAttrs would have to be called for both of them.
func (l *slogHandler) sinkWithCallDepth() LogSink {
	if sink, ok := l.sink.(CallDepthLogSink); ok {
		return sink.WithCallDepth(2)
	}
	return l.sink
}

func (l *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if l.sink == nil || len(attrs) == 0 {
		return l
	}

	clone := *l
	if l.slogSink != nil {
		clone.slogSink = l.slogSink.WithAttrs(attrs)
		clone.sink = clone.slogSink
		return &clone
	}

	kvList := make([]any, 0, 2*len(attrs))
	for _, attr := range attrs {
		kvList = attrToKVs(attr, kvList)
	}
	n := len(clone.currentValues)
	clone.currentValues = append(clone.currentValues[:n:n], kvList...) // copy on write

	return &clone
}

func (l *slogHandler) WithGroup(name string) slog.Handler {
	if l.sink == nil {
		return l
	}

	clone := *l
	if l.slogSink != nil {
		clone.slogSink = l.slogSink.WithGroup(name)
		clone.sink = clone.slogSink
		return &clone
	}

	// If the group name is empty, values are inlined.
	if name == "" {
		return &clone
	}

	// The first time we get a group with a non-empty name, we initialize
	// the tree of maps.  This means that all of the group fields are set or
	// unset together.

	currentMap := listToMap(clone.currentValues)
	if clone.groupName == "" {
		// We don't have a root yet, so the current values are it.
		clone.rootValues = currentMap
	} else {
		// The current values become a member of the parent.
		//FIXME: shared value, need a deep copy?
		clone.parentValues[clone.groupName] = currentMap
	}
	clone.parentValues = currentMap
	clone.currentValues = nil
	clone.groupName = name

	return &clone
}

// attrToKVs appends a slog.Attr to a logr-style kvList.  It handle slog Groups
// and other details of slog.
func attrToKVs(attr slog.Attr, kvList []any) []any {
	attrVal := attr.Value.Resolve()
	if attrVal.Kind() == slog.KindGroup {
		groupVal := attrVal.Group()
		grpKVs := make([]any, 0, 2*len(groupVal))
		for _, attr := range groupVal {
			grpKVs = attrToKVs(attr, grpKVs)
		}
		if attr.Key == "" {
			// slog says we have to inline these.
			kvList = append(kvList, grpKVs...)
		} else {
			// Convert the list into a map for rendering.
			kvList = append(kvList, attr.Key, listToMap(grpKVs))
		}
	} else if attr.Key != "" {
		kvList = append(kvList, attr.Key, attrVal.Any())
	}

	return kvList
}

func listToMap(kvList []any) map[string]any {
	kvMap := map[string]any{}
	for i := 0; i < len(kvList); i += 2 {
		k := kvList[i].(string) //nolint:forcetypeassert
		v := kvList[i+1]
		kvMap[k] = v
	}
	return kvMap
}

func mapToList(kvMap map[string]any) []any {
	kvList := make([]any, 0, 2*len(kvMap))
	for k, v := range kvMap {
		kvList = append(kvList, k, v)
	}
	return kvList
}

// levelFromSlog adjusts the level by the logger's verbosity and negates it.
// It ensures that the result is >= 0. This is necessary because the result is
// passed to a LogSink and that API did not historically document whether
// levels could be negative or what that meant.
//
// Some example usage:
//
//	logrV0 := getMyLogger()
//	logrV2 := logrV0.V(2)
//	slogV2 := slog.New(logr.ToSlogHandler(logrV2))
//	slogV2.Debug("msg") // =~ logrV2.V(4) =~ logrV0.V(6)
//	slogV2.Info("msg")  // =~  logrV2.V(0) =~ logrV0.V(2)
//	slogv2.Warn("msg")  // =~ logrV2.V(-4) =~ logrV0.V(0)
func (l *slogHandler) levelFromSlog(level slog.Level) int {
	result := -level
	result += l.levelBias // in case the original Logger had a V level
	if result < 0 {
		result = 0 // because LogSink doesn't expect negative V levels
	}
	return int(result)
}
