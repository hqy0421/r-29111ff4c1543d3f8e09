package customfuncs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jf-tech/omniparser/transformctx"
)

// lookupKeySep separates the parts of a composite lookup key. It is the ASCII unit-separator
// control character, which realistically never appears in catalog data, so composite keys are
// unambiguous: LookupKey("ab", "c") != LookupKey("a", "bc").
const lookupKeySep = "\x1f"

// LookupKey joins lookup key parts (e.g. supplier code and original SKU) into one unambiguous
// composite key. Both table registration (LookupRegistry.Register) and lookup (VersionedLookup)
// build keys with this function, so keys built from the same parts always match. Composite keys
// are essential when different suppliers may share the same code.
func LookupKey(keyParts ...string) string {
	return strings.Join(keyParts, lookupKeySep)
}

// LookupMiss describes one unknown lookup key encountered during a transform: the key was not
// present in the pinned-version reference table. Misses are collected in the LookupRegistry so
// the caller can emit them, after the transform, as a supplementary/backfill result. Note a
// table entry that exists but maps to an empty value is a KNOWN empty mapping, not a miss, and
// is never recorded here.
type LookupMiss struct {
	// Table is the name of the reference table the lookup targeted.
	Table string `json:"table"`
	// Version is the pinned dictionary version the lookup targeted.
	Version string `json:"version"`
	// Key is the composite lookup key (LookupKey of KeyParts).
	Key string `json:"key"`
	// KeyParts are the original key parts (e.g. supplier, SKU) for readability/backfill.
	KeyParts []string `json:"key_parts"`
	// Count is how many times this unknown key was looked up during the transform.
	Count int `json:"count"`
}

// LookupRegistry holds batch-registered, version-pinned reference tables for VersionedLookup
// and collects unknown-key misses during a transform. The intended usage is: create one
// registry per batch, register the reference table(s) for that batch, pass the registry to
// the transform via transformctx.Ctx.CustomParam, and after the transform read Misses() to
// produce the supplementary/backfill result. It is safe for concurrent use.
type LookupRegistry struct {
	mu        sync.Mutex
	tables    map[string]map[string]map[string]string // table -> version -> key -> value
	misses    []LookupMiss
	missIndex map[string]int // dedup key -> index into misses
}

// NewLookupRegistry creates an empty LookupRegistry.
func NewLookupRegistry() *LookupRegistry {
	return &LookupRegistry{
		tables:    map[string]map[string]map[string]string{},
		missIndex: map[string]int{},
	}
}

// Register registers one version of a reference table. entries maps lookup keys (built with
// LookupKey) to their mapped values; a mapped value may legitimately be empty (a known empty
// mapping). The entries map is copied, so later caller-side mutations don't affect the
// registry. Registering the same (table, version) twice is an error, so a batch can never
// silently overwrite a pinned dictionary.
func (r *LookupRegistry) Register(table, version string, entries map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tables[table] == nil {
		r.tables[table] = map[string]map[string]string{}
	}
	if _, dup := r.tables[table][version]; dup {
		return fmt.Errorf("reference table '%s' version '%s' is already registered", table, version)
	}
	copied := make(map[string]string, len(entries))
	for k, v := range entries {
		copied[k] = v
	}
	r.tables[table][version] = copied
	return nil
}

// Misses returns a snapshot copy of the unknown-key misses collected so far, in first-seen
// order. This is the supplementary/backfill result of a batch; it contains only genuinely
// unknown keys, never known-but-empty mappings. Call it after the transform completes.
func (r *LookupRegistry) Misses() []LookupMiss {
	r.mu.Lock()
	defer r.mu.Unlock()
	misses := make([]LookupMiss, len(r.misses))
	for i, m := range r.misses {
		m.KeyParts = append([]string(nil), m.KeyParts...)
		misses[i] = m
	}
	return misses
}

func (r *LookupRegistry) lookup(table, version, key string, keyParts []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions, found := r.tables[table]
	if !found {
		return "", fmt.Errorf("reference table '%s' is not registered", table)
	}
	entries, found := versions[version]
	if !found {
		available := make([]string, 0, len(versions))
		for v := range versions {
			available = append(available, v)
		}
		sort.Strings(available)
		return "", fmt.Errorf(
			"reference table '%s' has no version '%s' registered (available versions: [%s]): "+
				"pinned-version lookup never falls back to a different version",
			table, version, strings.Join(available, ", "))
	}
	if value, found := entries[key]; found {
		// Known mapping. Note the mapped value may legitimately be empty (a known empty
		// category) - that is NOT a miss and must not go into the supplementary result.
		return value, nil
	}
	// Unknown key: record it into the supplementary result, deduplicated by
	// (table, version, key) with a hit count.
	dedupKey := LookupKey(table, version, key)
	if index, found := r.missIndex[dedupKey]; found {
		r.misses[index].Count++
	} else {
		r.missIndex[dedupKey] = len(r.misses)
		r.misses = append(r.misses, LookupMiss{
			Table:    table,
			Version:  version,
			Key:      key,
			KeyParts: append([]string(nil), keyParts...),
			Count:    1,
		})
	}
	return "", nil
}

// LookupRegistryCarrier can be implemented by a caller's custom transformctx.Ctx.CustomParam
// type to carry a LookupRegistry alongside other batch state.
type LookupRegistryCarrier interface {
	GetLookupRegistry() *LookupRegistry
}

func lookupRegistryFrom(ctx *transformctx.Ctx) (*LookupRegistry, error) {
	var param interface{}
	if ctx != nil {
		param = ctx.CustomParam
	}
	switch p := param.(type) {
	case *LookupRegistry:
		return p, nil
	case LookupRegistryCarrier:
		if reg := p.GetLookupRegistry(); reg != nil {
			return reg, nil
		}
	}
	return nil, fmt.Errorf(
		"versionedLookup requires transformctx.Ctx.CustomParam to be a *customfuncs.LookupRegistry "+
			"(or to implement customfuncs.LookupRegistryCarrier), got %T", param)
}

// VersionedLookup looks up a composite key (keyParts joined by LookupKey, e.g. supplier code +
// original SKU) in the batch-registered reference table 'table' at the pinned version
// 'version', and returns the mapped value (e.g. the standard category).
//
// The registry is taken from transformctx.Ctx.CustomParam, which must be a *LookupRegistry
// (or implement LookupRegistryCarrier). The version is pinned: if the table or that exact
// version is not registered, an error is returned - the lookup never silently falls back to
// another version, so results are reproducible. To make the output self-describing, echo the
// version into the output record with the same decl used for the version arg, e.g.:
//
//	"std_category": { "custom_func": { "name": "versionedLookup", "args": [
//	    { "const": "sku_category" },
//	    { "external": "sku_category_dict_version" },
//	    { "xpath": "SUPPLIER" },
//	    { "xpath": "SKU" }
//	]}},
//	"std_category_dict_version": { "external": "sku_category_dict_version" }
//
// Outcome handling:
//   - key found with a non-empty value: the value is returned.
//   - key found with an empty value (a known empty mapping): "" is returned and nothing is
//     recorded - known empty mappings never mix into the supplementary result.
//   - key not found (an unknown code): "" is returned and the key is recorded in the
//     registry's supplementary result (see LookupRegistry.Misses), deduplicated by
//     (table, version, key) with a hit count.
//   - all key parts empty (e.g. the SKU field is absent in the input record): "" is returned
//     and nothing is looked up or recorded.
func VersionedLookup(ctx *transformctx.Ctx, table, version string, keyParts ...string) (string, error) {
	reg, err := lookupRegistryFrom(ctx)
	if err != nil {
		return "", err
	}
	allEmpty := true
	for _, kp := range keyParts {
		if kp != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		return "", nil
	}
	return reg.lookup(table, version, LookupKey(keyParts...), keyParts)
}
