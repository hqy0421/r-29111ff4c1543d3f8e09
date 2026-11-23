package customfuncs

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jf-tech/omniparser/transformctx"
)

func TestLookupKey(t *testing.T) {
	// Composite keys must be unambiguous: different part splits must not collide.
	assert.NotEqual(t, LookupKey("ab", "c"), LookupKey("a", "bc"))
	// Same parts always produce the same key.
	assert.Equal(t, LookupKey("SUP_A", "SKU-100"), LookupKey("SUP_A", "SKU-100"))
	// Different parts produce different keys.
	assert.NotEqual(t, LookupKey("SUP_A", "SKU-100"), LookupKey("SUP_B", "SKU-100"))
	// Single part key has no separator.
	assert.Equal(t, "SKU-100", LookupKey("SKU-100"))
	// Empty parts are preserved positionally: ("", "SKU-100") != ("SKU-100", "").
	assert.NotEqual(t, LookupKey("", "SKU-100"), LookupKey("SKU-100", ""))
}

func TestLookupRegistryRegister(t *testing.T) {
	reg := NewLookupRegistry()
	entries := map[string]string{LookupKey("SUP_A", "SKU-100"): "Tools"}
	assert.NoError(t, reg.Register("sku_category", "2026-Q4", entries))
	// The registry copies entries: mutating the caller's map afterwards has no effect.
	entries[LookupKey("SUP_A", "SKU-100")] = "Corrupted"
	v, err := VersionedLookup(
		&transformctx.Ctx{CustomParam: reg}, "sku_category", "2026-Q4", "SUP_A", "SKU-100")
	assert.NoError(t, err)
	assert.Equal(t, "Tools", v)
	// Re-registering the same (table, version) is an error.
	assert.Error(t, reg.Register("sku_category", "2026-Q4", map[string]string{}))
	// A different version of the same table is fine.
	assert.NoError(t, reg.Register("sku_category", "2026-Q3", map[string]string{}))
}

func ctxWithReg(reg *LookupRegistry) *transformctx.Ctx {
	return &transformctx.Ctx{CustomParam: reg}
}

func TestVersionedLookup(t *testing.T) {
	newReg := func() *LookupRegistry {
		reg := NewLookupRegistry()
		assert.NoError(t, reg.Register("sku_category", "2026-Q4", map[string]string{
			LookupKey("SUP_A", "SKU-100"): "Tools/Hand Tools",
			LookupKey("SUP_A", "SKU-200"): "", // known empty mapping
			LookupKey("SUP_B", "SKU-100"): "Outdoor/Camping",
		}))
		assert.NoError(t, reg.Register("sku_category", "2026-Q3", map[string]string{
			LookupKey("SUP_A", "SKU-100"): "Hardware",
		}))
		return reg
	}

	t.Run("hit returns mapped value of the pinned version", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_A", "SKU-100")
		assert.NoError(t, err)
		assert.Equal(t, "Tools/Hand Tools", v)
		assert.Empty(t, reg.Misses())
	})

	t.Run("pinned version is used exactly, no fallback to another version", func(t *testing.T) {
		reg := newReg()
		// Same key maps differently in 2026-Q3: proves the pinned version is the one used.
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q3", "SUP_A", "SKU-100")
		assert.NoError(t, err)
		assert.Equal(t, "Hardware", v)
		// A key that exists only in 2026-Q4 is a miss under 2026-Q3, not a fallback.
		v, err = VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q3", "SUP_B", "SKU-100")
		assert.NoError(t, err)
		assert.Equal(t, "", v)
		assert.Equal(t, []LookupMiss{{
			Table:    "sku_category",
			Version:  "2026-Q3",
			Key:      LookupKey("SUP_B", "SKU-100"),
			KeyParts: []string{"SUP_B", "SKU-100"},
			Count:    1,
		}}, reg.Misses())
	})

	t.Run("same code from different suppliers maps independently", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_B", "SKU-100")
		assert.NoError(t, err)
		assert.Equal(t, "Outdoor/Camping", v)
	})

	t.Run("known empty mapping returns empty and is not a miss", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_A", "SKU-200")
		assert.NoError(t, err)
		assert.Equal(t, "", v)
		assert.Empty(t, reg.Misses())
	})

	t.Run("unknown key returns empty and goes into the supplementary result", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_C", "SKU-999")
		assert.NoError(t, err)
		assert.Equal(t, "", v)
		misses := reg.Misses()
		assert.Equal(t, 1, len(misses))
		assert.Equal(t, "sku_category", misses[0].Table)
		assert.Equal(t, "2026-Q4", misses[0].Version)
		assert.Equal(t, []string{"SUP_C", "SKU-999"}, misses[0].KeyParts)
		assert.Equal(t, 1, misses[0].Count)
	})

	t.Run("repeated unknown keys are deduplicated with a count", func(t *testing.T) {
		reg := newReg()
		for i := 0; i < 3; i++ {
			_, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_C", "SKU-999")
			assert.NoError(t, err)
		}
		_, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_C", "SKU-998")
		assert.NoError(t, err)
		misses := reg.Misses()
		assert.Equal(t, 2, len(misses))
		assert.Equal(t, LookupKey("SUP_C", "SKU-999"), misses[0].Key)
		assert.Equal(t, 3, misses[0].Count)
		assert.Equal(t, LookupKey("SUP_C", "SKU-998"), misses[1].Key)
		assert.Equal(t, 1, misses[1].Count)
	})

	t.Run("unregistered table is an error", func(t *testing.T) {
		reg := newReg()
		_, err := VersionedLookup(ctxWithReg(reg), "no_such_table", "2026-Q4", "SUP_A", "SKU-100")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reference table 'no_such_table' is not registered")
	})

	t.Run("unregistered version is an error listing available versions", func(t *testing.T) {
		reg := newReg()
		_, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2099-Q1", "SUP_A", "SKU-100")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "has no version '2099-Q1' registered")
		assert.Contains(t, err.Error(), "2026-Q3")
		assert.Contains(t, err.Error(), "2026-Q4")
	})

	t.Run("all key parts empty skips lookup entirely", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "", "")
		assert.NoError(t, err)
		assert.Equal(t, "", v)
		assert.Empty(t, reg.Misses())
	})

	t.Run("partially empty key is still looked up", func(t *testing.T) {
		reg := newReg()
		v, err := VersionedLookup(ctxWithReg(reg), "sku_category", "2026-Q4", "SUP_C", "")
		assert.NoError(t, err)
		assert.Equal(t, "", v)
		misses := reg.Misses()
		assert.Equal(t, 1, len(misses))
		assert.Equal(t, []string{"SUP_C", ""}, misses[0].KeyParts)
	})

	t.Run("missing registry is an error", func(t *testing.T) {
		_, err := VersionedLookup(&transformctx.Ctx{}, "sku_category", "2026-Q4", "SUP_A", "SKU-100")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "LookupRegistry")
		_, err = VersionedLookup(nil, "sku_category", "2026-Q4", "SUP_A", "SKU-100")
		assert.Error(t, err)
		_, err = VersionedLookup(
			&transformctx.Ctx{CustomParam: "not a registry"}, "sku_category", "2026-Q4", "A", "B")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "string")
	})
}

type testLookupRegistryCarrier struct{ reg *LookupRegistry }

func (c testLookupRegistryCarrier) GetLookupRegistry() *LookupRegistry { return c.reg }

func TestVersionedLookupRegistryCarrier(t *testing.T) {
	reg := NewLookupRegistry()
	assert.NoError(t, reg.Register("t", "v1", map[string]string{LookupKey("k"): "value"}))
	v, err := VersionedLookup(
		&transformctx.Ctx{CustomParam: testLookupRegistryCarrier{reg: reg}}, "t", "v1", "k")
	assert.NoError(t, err)
	assert.Equal(t, "value", v)
	// A carrier returning nil is an error.
	_, err = VersionedLookup(
		&transformctx.Ctx{CustomParam: testLookupRegistryCarrier{reg: nil}}, "t", "v1", "k")
	assert.Error(t, err)
}

func TestLookupRegistryMissesReturnsSnapshotCopy(t *testing.T) {
	reg := NewLookupRegistry()
	assert.NoError(t, reg.Register("t", "v1", map[string]string{}))
	_, err := VersionedLookup(ctxWithReg(reg), "t", "v1", "unknown")
	assert.NoError(t, err)
	misses := reg.Misses()
	assert.Equal(t, 1, len(misses))
	// Mutating the returned snapshot must not affect the registry.
	misses[0].Count = 999
	misses[0].KeyParts[0] = "corrupted"
	fresh := reg.Misses()
	assert.Equal(t, 1, fresh[0].Count)
	assert.Equal(t, []string{"unknown"}, fresh[0].KeyParts)
}

func TestVersionedLookupConcurrent(t *testing.T) {
	reg := NewLookupRegistry()
	assert.NoError(t, reg.Register("t", "v1", map[string]string{LookupKey("hit"): "value"}))
	const goroutines = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := VersionedLookup(ctxWithReg(reg), "t", "v1", "hit")
			assert.NoError(t, err)
			_, err = VersionedLookup(ctxWithReg(reg), "t", "v1", "miss")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	misses := reg.Misses()
	assert.Equal(t, 1, len(misses))
	assert.Equal(t, goroutines, misses[0].Count)
}
