package manifest

import (
	"testing"

	"github.com/Masterminds/semver/v3"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// A host built from this kernel must satisfy the range overlays declare
// (kernel >= v3.OverlayMinKernel); a 3.0.x host must not.
func TestAPIVersionUnderstandsOverlays(t *testing.T) {
	c, _ := semver.NewConstraint(">=" + v3.OverlayMinKernel + " <4.0.0")
	if !c.Check(semver.MustParse(APIVersion)) {
		t.Fatalf("APIVersion %s does not satisfy kernel >=%s", APIVersion, v3.OverlayMinKernel)
	}
	if c.Check(semver.MustParse("3.0.0")) {
		t.Fatal("a 3.0.0 host would accept an overlay")
	}
}
