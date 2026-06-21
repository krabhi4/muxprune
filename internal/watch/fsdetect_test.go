package watch

import "testing"

func TestIsNetworkFSType(t *testing.T) {
	network := map[string]int64{
		"nfs":  0x6969,
		"smb":  0x517B,
		"smb2": 0xFE534D42,
		"cifs": 0xFF534D42,
		"fuse": 0x65735546,
	}
	local := map[string]int64{
		"ext4":    0xEF53,
		"xfs":     0x58465342,
		"btrfs":   0x9123683E,
		"tmpfs":   0x01021994,
		"zfs":     0x2FC12FC1,
		"zero":    0x0,
		"overlay": 0x794C7630,
	}
	for name, magic := range network {
		if !isNetworkFSType(magic) {
			t.Errorf("%s (0x%X) should be classified as network", name, magic)
		}
	}
	for name, magic := range local {
		if isNetworkFSType(magic) {
			t.Errorf("%s (0x%X) should be classified as local", name, magic)
		}
	}
}
