package buildinfo

// Info holds build-time metadata, populated from main via -ldflags.
type Info struct {
	SHA  string
	Time string
}
