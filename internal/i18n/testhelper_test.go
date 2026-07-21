package i18n

import "os"

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return b
}
