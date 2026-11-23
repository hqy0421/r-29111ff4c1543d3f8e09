package tablelookup

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bradleyjkemp/cupaloy"
	"github.com/jf-tech/go-corelib/jsons"
	"github.com/stretchr/testify/assert"

	"github.com/jf-tech/omniparser"
	"github.com/jf-tech/omniparser/customfuncs"
	"github.com/jf-tech/omniparser/extensions/omniv21"
	"github.com/jf-tech/omniparser/transformctx"
)

// TestProductCatalogVersionedLookup demonstrates a product-catalog conversion where original
// SKUs are mapped to standard categories via a batch-registered, version-pinned reference
// table. Because different suppliers may share the same code, the lookup key is composite:
// (supplier, SKU).
func TestProductCatalogVersionedLookup(t *testing.T) {
	// The batch registers its reference table(s) with pinned versions before the transform.
	// Two versions of the same table are registered; the transform below pins one of them
	// through an external property, and the lookup never falls back to the other version.
	registry := customfuncs.NewLookupRegistry()
	assert.NoError(t, registry.Register("sku_category", "2026-Q3", map[string]string{
		customfuncs.LookupKey("SUP_A", "SKU-100"): "Hardware",
	}))
	assert.NoError(t, registry.Register("sku_category", "2026-Q4", map[string]string{
		customfuncs.LookupKey("SUP_A", "SKU-100"): "Tools/Hand Tools",
		customfuncs.LookupKey("SUP_A", "SKU-200"): "", // known empty category: NOT a miss
		customfuncs.LookupKey("SUP_B", "SKU-100"): "Outdoor/Camping",
	}))

	schemaFile := "./product_catalog.schema.json"
	schemaFileReader, err := os.Open(schemaFile)
	assert.NoError(t, err)
	defer schemaFileReader.Close()

	inputFile := "./product_catalog.input.csv"
	inputFileReader, err := os.Open(inputFile)
	assert.NoError(t, err)
	defer inputFileReader.Close()

	schema, err := omniparser.NewSchema(
		filepath.Base(schemaFile),
		schemaFileReader,
		omniparser.Extension{
			CreateSchemaHandler: omniv21.CreateSchemaHandler,
			CustomFuncs:         customfuncs.CommonCustomFuncs,
		})
	assert.NoError(t, err)
	transform, err := schema.NewTransform(
		filepath.Base(inputFile),
		inputFileReader,
		&transformctx.Ctx{
			// The pinned dictionary version for this batch. The schema uses it both as the
			// versionedLookup version arg and echoes it into every output record, so the
			// output records exactly which dictionary version produced it.
			ExternalProperties: map[string]string{"sku_category_dict_version": "2026-Q4"},
			CustomParam:        registry,
		})
	assert.NoError(t, err)

	var records []string
	for {
		recordBytes, err := transform.Read()
		if err == io.EOF {
			break
		}
		assert.NoError(t, err)
		records = append(records, string(recordBytes))
	}
	// Main output: SUP_A/SKU-100 maps via the pinned 2026-Q4 version (not 2026-Q3's
	// "Hardware"); SUP_B/SKU-100 maps independently of SUP_A's SKU-100; SUP_A/SKU-200 has a
	// known empty category so no std_category is emitted; unknown codes (SUP_B/SKU-999,
	// SUP_C/SKU-100) have no std_category either - they surface in the supplementary result
	// below instead.
	cupaloy.SnapshotMulti("records", jsons.BPJ("["+strings.Join(records, ",")+"]"))
	// Supplementary (backfill) result: only genuinely unknown codes - the known empty
	// mapping (SUP_A/SKU-200) and the keyless orphan row are NOT here - deduplicated with a
	// hit count, each entry stamped with the dictionary version that was in force.
	cupaloy.SnapshotMulti("supplementary", jsons.BPM(registry.Misses()))
}
