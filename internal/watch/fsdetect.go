package watch

const (
	magicNFS  = 0x6969
	magicSMB  = 0x517B
	magicSMB2 = 0xFE534D42
	magicCIFS = 0xFF534D42
	magicFUSE = 0x65735546
)

func isNetworkFSType(magic int64) bool {
	switch magic {
	case magicNFS, magicSMB, magicSMB2, magicCIFS, magicFUSE:
		return true
	}
	return false
}
