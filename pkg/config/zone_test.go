// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package config_test

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	proto "github.com/gogo/protobuf/proto"
	yaml "gopkg.in/yaml.v2"
)

func TestZoneConfigValidate(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := []struct {
		cfg      config.ZoneConfig
		expected string
	}{
		{
			config.ZoneConfig{},
			"attributes for at least one replica must be specified in zone config",
		},
		{
			config.ZoneConfig{
				NumReplicas: 2,
			},
			"at least 3 replicas are required for multi-replica configurations",
		},
		{
			config.ZoneConfig{
				NumReplicas: 1,
			},
			"RangeMaxBytes 0 less than minimum allowed",
		},
		{
			config.ZoneConfig{
				NumReplicas:   1,
				RangeMaxBytes: config.DefaultZoneConfig().RangeMaxBytes,
			},
			"",
		},
		{
			config.ZoneConfig{
				NumReplicas:   1,
				RangeMinBytes: config.DefaultZoneConfig().RangeMaxBytes,
				RangeMaxBytes: config.DefaultZoneConfig().RangeMaxBytes,
			},
			"is greater than or equal to RangeMaxBytes",
		},
	}
	for i, c := range testCases {
		err := c.cfg.Validate()
		if !testutils.IsError(err, c.expected) {
			t.Fatalf("%d: expected %q, got %v", i, c.expected, err)
		}
	}
}

// TestZoneConfigMarshalYAML makes sure that ZoneConfig is correctly marshaled
// to YAML and back.
func TestZoneConfigMarshalYAML(t *testing.T) {
	defer leaktest.AfterTest(t)()

	original := config.ZoneConfig{
		RangeMinBytes: 1,
		RangeMaxBytes: 1,
		GC: config.GCPolicy{
			TTLSeconds: 1,
		},
		NumReplicas: 1,
		Constraints: config.Constraints{
			Constraints: []config.Constraint{
				{
					Type:  config.Constraint_POSITIVE,
					Value: "foo",
				},
				{
					Type:  config.Constraint_REQUIRED,
					Key:   "duck",
					Value: "foo",
				},
				{
					Type:  config.Constraint_PROHIBITED,
					Key:   "duck",
					Value: "foo",
				},
			},
		},
	}

	expected := `range_min_bytes: 1
range_max_bytes: 1
gc:
  ttlseconds: 1
num_replicas: 1
constraints: [foo, +duck=foo, -duck=foo]
`

	body, err := yaml.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != expected {
		t.Fatalf("yaml.Marshal(%+v) = %s; not %s", original, body, expected)
	}

	var unmarshaled config.ZoneConfig
	if err := yaml.UnmarshalStrict(body, &unmarshaled); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&unmarshaled, &original) {
		t.Errorf("yaml.UnmarshalStrict(%q) = %+v; not %+v", body, unmarshaled, original)
	}
}

func TestZoneSpecifiers(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Simulate exactly two named zones: one named default and one named carl.
	// N.B. config.DefaultZoneName must always exist in the mapping; it is treated
	// specially so that it always appears first in the lookup path.
	defer func(old map[string]uint32) { config.NamedZones = old }(config.NamedZones)
	config.NamedZones = map[string]uint32{
		config.DefaultZoneName: 0,
		"carl":                 42,
	}
	defer func(old map[uint32]string) { config.NamedZonesByID = old }(config.NamedZonesByID)
	config.NamedZonesByID = map[uint32]string{
		0:  config.DefaultZoneName,
		42: "carl",
	}

	// Simulate the following schema:
	//   CREATE DATABASE db;   CREATE TABLE db.table ...
	//   CREATE DATABASE ".";  CREATE TABLE ".".".table." ...
	//   CREATE DATABASE carl; CREATE TABLE carl.toys ...
	type namespaceEntry struct {
		parentID uint32
		name     string
	}
	namespace := map[namespaceEntry]uint32{
		{0, "db"}:               50,
		{50, "tbl"}:             51,
		{0, "user"}:             52,
		{0, "."}:                53,
		{53, ".table."}:         54,
		{0, "carl"}:             55,
		{55, "toys"}:            56,
		{9000, "broken_parent"}: 57,
	}
	resolveName := func(parentID uint32, name string) (uint32, error) {
		key := namespaceEntry{parentID, name}
		if id, ok := namespace[key]; ok {
			return id, nil
		}
		return 0, fmt.Errorf("%q not found", name)
	}
	resolveID := func(id uint32) (parentID uint32, name string, err error) {
		for entry, entryID := range namespace {
			if id == entryID {
				return entry.parentID, entry.name, nil
			}
		}
		return 0, "", fmt.Errorf("%d not found", id)
	}

	for _, tc := range []struct {
		cliSpecifier string
		id           int
		err          string
	}{
		{".default", 0, ""},
		{".carl", 42, ""},
		{".foo", -1, `"foo" is not a built-in zone`},
		{"db", 50, ""},
		{".db", -1, `"db" is not a built-in zone`},
		{"db.tbl", 51, ""},
		{"tbl", -1, `"tbl" not found`},
		{"table", -1, `malformed name: "table"`}, // SQL keyword; requires quotes
		{`"table"`, -1, `"table" not found`},
		{"user", -1, "malformed name: \"user\""}, // SQL keyword; requires quotes
		{`"user"`, 52, ""},
		{`"."`, 53, ""},
		{`.`, -1, `missing zone name`},
		{`".table."`, -1, `".table." not found`},
		{`".".".table."`, 54, ""},
		{`.table.`, -1, `"table." is not a built-in zone`},
		{"carl", 55, ""},
		{"carl.toys", 56, ""},
		{"carl.love", -1, `"love" not found`},
		{"; DROP DATABASE system", -1, `malformed name`},
	} {
		t.Run(fmt.Sprintf("parse-cli=%s", tc.cliSpecifier), func(t *testing.T) {
			err := func() error {
				zs, err := config.ParseCLIZoneSpecifier(tc.cliSpecifier)
				if err != nil {
					return err
				}
				id, err := config.ResolveZoneSpecifier(zs, resolveName)
				if err != nil {
					return err
				}
				if e, a := tc.id, int(id); a != e {
					t.Errorf("path %d did not match expected path %d", a, e)
				}
				if e, a := tc.cliSpecifier, config.CLIZoneSpecifier(zs); e != a {
					t.Errorf("expected %q to roundtrip, but got %q", e, a)
				}
				return nil
			}()
			if !testutils.IsError(err, tc.err) {
				t.Errorf("expected error matching %q, but got %v", tc.err, err)
			}
		})
	}

	for _, tc := range []struct {
		id           uint32
		cliSpecifier string
		err          string
	}{
		{0, ".default", ""},
		{41, "", "41 not found"},
		{42, ".carl", ""},
		{50, "db", ""},
		{51, "db.tbl", ""},
		{52, `"user"`, ""},
		{53, `"."`, ""},
		{54, `".".".table."`, ""},
		{55, "carl", ""},
		{56, "carl.toys", ""},
		{57, "", "9000 not found"},
		{58, "", "58 not found"},
	} {
		t.Run(fmt.Sprintf("resolve-id=%d", tc.id), func(t *testing.T) {
			zs, err := config.ZoneSpecifierFromID(tc.id, resolveID)
			if !testutils.IsError(err, tc.err) {
				t.Errorf("unable to lookup ID %d: %s", tc.id, err)
			}
			if tc.err != "" {
				return
			}
			if e, a := tc.cliSpecifier, config.CLIZoneSpecifier(zs); e != a {
				t.Errorf("expected %q specifier for ID %d, but got %q", e, tc.id, a)
			}
		})
	}
}
