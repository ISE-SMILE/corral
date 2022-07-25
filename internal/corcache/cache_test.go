package corcache

import (
	"testing"

	"github.com/ISE-SMILE/corral/api"
)

func Test_InitCache(t *testing.T) {
	tests := []struct {
		input api.CacheSystemType
	}{{api.InMemory}}

	for i, test := range tests {
		cs, err := NewCacheSystem(test.input)
		if err != nil {
			t.Fatalf("failed to init cache for test %d with type %d", i, test.input)
		}
		if cs == nil {
			t.Fatalf("Expected non-nil cache return for test %d", i)
		}
	}
}

func Test_CacheSystemTypes(t *testing.T) {
	tests := []struct {
		impl     api.CacheSystem
		expected api.CacheSystemType
	}{
		{NewLocalInMemoryProvider(10), api.InMemory},
		//TODO: add each new CachImpl here
	}

	for i, test := range tests {
		if CacheSystemTypes(test.impl) != test.expected {
			t.Fatalf("failed test %d could not match %+v to %d", i, test.impl, test.expected)
		}
	}
}
