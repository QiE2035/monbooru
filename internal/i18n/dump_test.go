package i18n

import (
	"fmt"
	"testing"

	"github.com/BurntSushi/toml"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func TestDumpKeys(t *testing.T) {
	b := goi18n.NewBundle(language.Make("en"))
	b.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	mf, err := b.ParseMessageFileBytes(mustRead("locales/en.toml"), "locales/en.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mf.Messages {
		fmt.Printf("ID=%q other=%q\n", m.ID, m.Other)
	}
}
