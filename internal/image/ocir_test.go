package image

import "testing"

func TestSplitOCIRRef(t *testing.T) {
	name, tag, err := splitOCIRRef("ocir.eu-frankfurt-1.oci.oraclecloud.com/ns/cloudsourced-env:abc-amd64")
	if err != nil || name != "cloudsourced-env" || tag != "abc-amd64" {
		t.Fatalf("got %q %q %v", name, tag, err)
	}
	if _, _, err := splitOCIRRef("host/name:tag"); err == nil {
		t.Fatal("want an error without namespace")
	}
}
