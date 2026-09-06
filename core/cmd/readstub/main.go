package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/fxamacker/cbor/v2"
	"github.com/jm33-m0/emp3r0r/core/internal/def"
	"github.com/jm33-m0/emp3r0r/core/lib/crypto"
)

func main() {
	bin, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	base := bin
	// try every offset shift -16..+16 and every trim of trailing zeros/ff
	for shift := -32; shift <= 32; shift++ {
		start := shift
		if start < 0 {
			continue
		}
		if start >= len(base) {
			break
		}
		w := base[start:]
		for _, trim := range []byte{0x00, 0xff} {
			t := bytes.TrimRight(w, string(trim))
			if len(t) < 64 {
				continue
			}
			dec, err := crypto.AES_GCM_Decrypt([]byte(def.MagicString), t)
			if err != nil {
				continue
			}
			var cfg def.Config
			if err := cbor.Unmarshal(dec, &cfg); err == nil {
				fmt.Printf("OK shift=%d trim=%#x\n", shift, trim)
				fmt.Printf("CCAddress:     %s\nC2ChannelMode: %s\nRelayURLs:     %d\n", cfg.CCAddress, cfg.C2ChannelMode, len(cfg.RelayURLs))
				for i, u := range cfg.RelayURLs {
					if len(u) > 90 {
						u = u[:90] + "..."
					}
					fmt.Printf("  [%d] %s\n", i, u)
				}
				return
			}
		}
	}
	fmt.Println("no valid window found")
}
