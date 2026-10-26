package web

import (
	"errors"
	"testing"

	"phenix/util/mm"
)

// imagePathTestMM reports a fixed minimega files directory.
type imagePathTestMM struct {
	mm.MM
}

func (imagePathTestMM) GetMMArgs() (map[string]string, error) {
	return map[string]string{"filepath": "/phenix/images"}, nil
}

func TestValidateImagePath(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = imagePathTestMM{} //nolint:reassign // install test double

	tests := map[string]struct {
		image   string
		current []string
		wantErr error
	}{
		"empty means unchanged":       {image: ""},
		"relative image":              {image: "ubuntu.qc2"},
		"relative image with cache":   {image: "ubuntu.qc2,writeback"},
		"relative image in subdir":    {image: "base/ubuntu.qc2"},
		"absolute image in files dir": {image: "/phenix/images/ubuntu.qc2"},
		"iso in files dir":            {image: "/phenix/images/install.iso"},
		"current image elsewhere":     {image: "/data/images/legacy.qc2", current: []string{"/data/images/legacy.qc2"}},
		"host block device":           {image: "/dev/sda", wantErr: errImageOutsideFiles},
		"host file":                   {image: "/etc/shadow", wantErr: errImageOutsideFiles},
		"host file with cache":        {image: "/etc/shadow,writeback", wantErr: errImageOutsideFiles},
		"relative climb":              {image: "../../etc/shadow", wantErr: errImageOutsideFiles},
		"absolute climb":              {image: "/phenix/images/../../etc/shadow", wantErr: errImageOutsideFiles},
		"sibling prefix":              {image: "/phenix/images-old/x.qc2", wantErr: errImageOutsideFiles},
		"files dir itself":            {image: "/phenix/images", wantErr: errImageOutsideFiles},
		"other image than current": {
			image:   "/data/images/other.qc2",
			current: []string{"/data/images/legacy.qc2"},
			wantErr: errImageOutsideFiles,
		},
		"own experiment files":      {image: "exp1/files/snap.qc2"},
		"other experiment files":    {image: "exp2/files/snap.qc2", wantErr: errImageOtherExp},
		"other experiment absolute": {image: "/phenix/images/exp2/files/snap.qc2", wantErr: errImageOtherExp},
		"env var expansion":         {image: "$HOME/x.qc2", wantErr: errImageUnsafe},
		"env var in subdir":         {image: "a/${PWD}.qc2", wantErr: errImageUnsafe},
		"env var in cache mode":     {image: "ubuntu.qc2,$X", wantErr: errImageUnsafe},
		"http url":                  {image: "http://example.test/x.qc2", wantErr: errImageUnsafe},
		"https url":                 {image: "https://example.test/x.qc2", wantErr: errImageUnsafe},
		"file prefix":               {image: "file:x.qc2", wantErr: errImageUnsafe},
		"tar prefix":                {image: "tar:x.tar", wantErr: errImageUnsafe},
		"quote":                     {image: "a\"b.qc2", wantErr: errImageUnsafe},
		"comment":                   {image: "a#b.qc2", wantErr: errImageUnsafe},
		"newline":                   {image: "a\nb.qc2", wantErr: errImageUnsafe},
		"topology image elsewhere":  {image: "/data/base.qc2", current: []string{"/data/base.qc2,writeback"}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateImagePath("exp1", tc.image, tc.current...)

			if tc.wantErr == nil && err != nil {
				t.Fatalf("validateImagePath(%q) error = %v, want nil", tc.image, err)
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateImagePath(%q) error = %v, want %v", tc.image, err, tc.wantErr)
			}
		})
	}
}

func TestValidateISOPathRejectsComma(t *testing.T) {
	if err := validateISOPath("exp1", "vm1", "install.iso,/etc/shadow"); !errors.Is(err, errImageUnsafe) {
		t.Fatalf("validateISOPath error = %v, want %v", err, errImageUnsafe)
	}
}
