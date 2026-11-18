package compress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"sort"
	"testing"
)

// The bytes a codec produces are part of the deduplication contract across
// releases, not only within one: a new backup reuses the blobs of an old one
// only if the same chunk compresses to the same bytes. The determinism tests
// compare one build with itself, so a library upgrade that changes the output
// passes them, and the next --dedup run quietly uploads everything again.
//
// goldenCompressed pins the output of the libraries go.mod requires. A change
// here is a decision, not a test to fix: the CHANGELOG has to say that
// deduplication against older backups restarts, and the failure message
// prints the entries to paste. TestGoldenPinsMatchDependencyVersion makes a
// library bump fail even when the digests happen not to move.
func TestCodecOutputMatchesThePinnedDigests(t *testing.T) {
	fixtures := determinismFixtures(1<<12, 1<<20)
	checked := 0
	for _, name := range Names() {
		codec, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, level := range goldenLevels(codec) {
			for fixture, data := range fixtures {
				key := fmt.Sprintf("%s/%d/%s", name, level, fixture)
				sum := sha256.Sum256(compressOnce(t, codec, level, data))
				got := hex.EncodeToString(sum[:])
				want, ok := goldenCompressed[key]
				switch {
				case !ok:
					t.Errorf("no pinned digest; add\n\t%q: %q,", key, got)
				case got != want:
					t.Errorf("%s compresses to different bytes than the pinned library; "+
						"if the upgrade is intended, note in the CHANGELOG that --dedup "+
						"re-uploads, and pin\n\t%q: %q,", key, key, got)
				}
				checked++
			}
		}
	}
	if checked != len(goldenCompressed) {
		t.Errorf("checked %d outputs, %d are pinned: remove the entries of codecs or levels that no longer exist",
			checked, len(goldenCompressed))
	}
}

// goldenLevels is each codec's lowest, default and highest level.
func goldenLevels(c Codec) []int {
	minLevel, maxLevel, def := c.Levels()
	set := map[int]bool{minLevel: true, def: true, maxLevel: true}
	levels := make([]int, 0, len(set))
	for level := range set {
		levels = append(levels, level)
	}
	sort.Ints(levels)
	return levels
}

// goldenCompressed: codec/level/fixture -> sha256 of the compressed bytes.
var goldenCompressed = map[string]string{
	"gzip/1/noise-1048576":  "4f6b5ff642b77268e5d6d2e4942041ad7cf784010b03497b68278efe3b804872",
	"gzip/1/noise-4096":     "e8a49dfa643343c5425926c03eb1e283b48a71ea12f30ac9ee3415e6f48d9c8a",
	"gzip/1/text-1048576":   "b5e15ec63a3cf0fa4abfbc417d9175afc06cc9b17fd9c56fe30b94f99623a988",
	"gzip/1/text-4096":      "3bb0fd7cb88df884e7ab7309ad31a4dc09970952491d87db7878f435f105be4a",
	"gzip/1/zeros-1048576":  "f9c45f0a7d7391ae91ec1ddcb9201d633f9bbe40ad52e1ea7ada0d8a44f8d6f3",
	"gzip/1/zeros-4096":     "82da27d488d11d79618d2a6d5a89f872d2507ad822f7b23424033c11fd132f2f",
	"gzip/6/noise-1048576":  "5e3a453f744383d4b124e39338896158ce86b31415b2f31e4b653109ec4d578b",
	"gzip/6/noise-4096":     "9757e7791aeb176aaf5014bcf4657ee4e90112b77d6d74c474e449a370ba1ddc",
	"gzip/6/text-1048576":   "7a4eea85b83fb810963849b4bd2420485511fa2b00704e7742a49f8d51b79972",
	"gzip/6/text-4096":      "d7b221122352242697623715858b1afbcd1f2ed12f6292bef1ef566d006bdf10",
	"gzip/6/zeros-1048576":  "83c95878b4198fcb9fbe8ea034c5476377601fdce7b5715783adee9b9933149f",
	"gzip/6/zeros-4096":     "662cdaec9de87d4c2372e28309d2471932aa22035cf3d0ba82f55f52667d7837",
	"gzip/9/noise-1048576":  "0441a38d60f8e1831687f62da996e476bd46e5eca4797c9175041ae62a884968",
	"gzip/9/noise-4096":     "3d20266109f2c95705f12187856d84f7cc6528fef78ae546d6fb16b28fb02b5b",
	"gzip/9/text-1048576":   "0b2f8a11e0a9a0bebeb72cda86f7edea512b6d497c33b073d0e89c79927fb0f1",
	"gzip/9/text-4096":      "accbb39cafe587bf031ae90789c6b8caec0448c63ec1587d9fc0e4e7f67e0d3c",
	"gzip/9/zeros-1048576":  "d7851fe46f7c0b2b0fd180f32f26158818ddf3a026a8ecd9b6edd56fee5a2b29",
	"gzip/9/zeros-4096":     "651a6d48f1a20f7a933c2b41b4ca7db1ab2070d32173e720c7e17ab2537b5e2c",
	"lz4/0/noise-1048576":   "df2029f73a5905d6eb1deb5a803de1eed005b98a3b5c5f89f23167c4f7c49380",
	"lz4/0/noise-4096":      "ff6b838715444c97a263d46475355d368542288b75e7147ab2a63118817a3cc3",
	"lz4/0/text-1048576":    "05436186148017db34a4820976907c97a9a6f5b116d1b012d76e141333e02b27",
	"lz4/0/text-4096":       "84b6e3b752aab7e84e7013f3520108f62054d51434c7d24f8947ec56741172a7",
	"lz4/0/zeros-1048576":   "b7bd26085388984974c5461c4a4ac0c628c9ce66f5e5e81a15d5b34de58b9428",
	"lz4/0/zeros-4096":      "e6936a9476a12044cd961cee4abec7c33e0407e74688ebfcdfe72f3ffe78f143",
	"lz4/1/noise-1048576":   "df2029f73a5905d6eb1deb5a803de1eed005b98a3b5c5f89f23167c4f7c49380",
	"lz4/1/noise-4096":      "ff6b838715444c97a263d46475355d368542288b75e7147ab2a63118817a3cc3",
	"lz4/1/text-1048576":    "82254bc9b1156bde4fddcd4c13ccf10751d8e94f9f355bbe062e495936a73553",
	"lz4/1/text-4096":       "02ccc41e54f067f2cd844acf445e7520bedaf62865d674802e61cf27bdd5d37b",
	"lz4/1/zeros-1048576":   "5642ebae73b4cfe8bea0c9606784e12292ee10d3aef1fd5a6114dca05516b19e",
	"lz4/1/zeros-4096":      "53d47a378ca1c674709673c21f3d8b2392a93d3c9fb18ee494fe425622533acd",
	"lz4/9/noise-1048576":   "df2029f73a5905d6eb1deb5a803de1eed005b98a3b5c5f89f23167c4f7c49380",
	"lz4/9/noise-4096":      "ff6b838715444c97a263d46475355d368542288b75e7147ab2a63118817a3cc3",
	"lz4/9/text-1048576":    "d4c34e7bbbb0fe0c70357f99db24ae19b005e9995b95a56a533b63cd3fef9618",
	"lz4/9/text-4096":       "02ccc41e54f067f2cd844acf445e7520bedaf62865d674802e61cf27bdd5d37b",
	"lz4/9/zeros-1048576":   "5642ebae73b4cfe8bea0c9606784e12292ee10d3aef1fd5a6114dca05516b19e",
	"lz4/9/zeros-4096":      "53d47a378ca1c674709673c21f3d8b2392a93d3c9fb18ee494fe425622533acd",
	"store/0/noise-1048576": "aa9bf3251d0754379f51f5a7ca15835c504a1b1e8fa6663463c22dfc9ae527b8",
	"store/0/noise-4096":    "ce7bfc49cf883431b93d72773821c9851445ce1428041b802f44852e645692b8",
	"store/0/text-1048576":  "c7adee6477cc795a840814d595e2cb3cf9ba0ceb01b356c2b34426b812bfa252",
	"store/0/text-4096":     "3fe331598e7cca2534cf03ba21b42cc65d8df0de52984033aae64b81dd430259",
	"store/0/zeros-1048576": "30e14955ebf1352266dc2ff8067e68104607e750abb9d3b36582b8af909fcb58",
	"store/0/zeros-4096":    "ad7facb2586fc6e966c004d7d1d16b024f5805ff7cb47c7a85dabd8b48892ca7",
	"xz/0/noise-1048576":    "d65c957fdbd5651655450df4b8e9f6d6976d792a53e41abc8869f5426efb66f9",
	"xz/0/noise-4096":       "1c8f954586fa14295f706ec483b16b71265c3b2e4bf13cec22b2f2e6c20608f1",
	"xz/0/text-1048576":     "b3019194d420a77f242595c2af342e9a27619e62c6b0c101994dd96b0a9dd462",
	"xz/0/text-4096":        "29b884d94454dab551ddcd395ea4633798fbe1e8ee790cf781af6da209fd646c",
	"xz/0/zeros-1048576":    "d4b8d7ffff1a03cffd5ed6e955ae14ad91dda417d5a0d315cfe7e71f853a3885",
	"xz/0/zeros-4096":       "5624945294ddf8ed37dc559e2a8b2d0464a4dbf4b87dad399219c2e262131089",
	"xz/6/noise-1048576":    "d65c957fdbd5651655450df4b8e9f6d6976d792a53e41abc8869f5426efb66f9",
	"xz/6/noise-4096":       "1c8f954586fa14295f706ec483b16b71265c3b2e4bf13cec22b2f2e6c20608f1",
	"xz/6/text-1048576":     "b3019194d420a77f242595c2af342e9a27619e62c6b0c101994dd96b0a9dd462",
	"xz/6/text-4096":        "29b884d94454dab551ddcd395ea4633798fbe1e8ee790cf781af6da209fd646c",
	"xz/6/zeros-1048576":    "d4b8d7ffff1a03cffd5ed6e955ae14ad91dda417d5a0d315cfe7e71f853a3885",
	"xz/6/zeros-4096":       "5624945294ddf8ed37dc559e2a8b2d0464a4dbf4b87dad399219c2e262131089",
	"xz/9/noise-1048576":    "d65c957fdbd5651655450df4b8e9f6d6976d792a53e41abc8869f5426efb66f9",
	"xz/9/noise-4096":       "1c8f954586fa14295f706ec483b16b71265c3b2e4bf13cec22b2f2e6c20608f1",
	"xz/9/text-1048576":     "b3019194d420a77f242595c2af342e9a27619e62c6b0c101994dd96b0a9dd462",
	"xz/9/text-4096":        "29b884d94454dab551ddcd395ea4633798fbe1e8ee790cf781af6da209fd646c",
	"xz/9/zeros-1048576":    "d4b8d7ffff1a03cffd5ed6e955ae14ad91dda417d5a0d315cfe7e71f853a3885",
	"xz/9/zeros-4096":       "5624945294ddf8ed37dc559e2a8b2d0464a4dbf4b87dad399219c2e262131089",
	"zstd/1/noise-1048576":  "df8555248f0f13f2922ba9e4314a5170ec34ef4793790a820b2fe94ac70c44f7",
	"zstd/1/noise-4096":     "25fe655355d37dad452781e2857d7f721997bec9f127a5f2230d2865bf0412d3",
	"zstd/1/text-1048576":   "21407a9becac4149deed402f9586f7cab738652f8f79312fbee55ac53e64e546",
	"zstd/1/text-4096":      "a4b00a95bea13bfce97d7069b2927388981fe3cb23c36f60e0fa9eaa7c652d95",
	"zstd/1/zeros-1048576":  "8d02ada5b391ea3ab520818c320d0d3d95ce7e974ea22680f90962ebd3e429e7",
	"zstd/1/zeros-4096":     "3f6eb289782a5e7edc82aae9f37d424a6fe76171fc0ee720459c931071391e40",
	"zstd/2/noise-1048576":  "2ea0659ac52388fb7919c659eda3b8e330a56122f3f6bda01c293eb326b62737",
	"zstd/2/noise-4096":     "25fe655355d37dad452781e2857d7f721997bec9f127a5f2230d2865bf0412d3",
	"zstd/2/text-1048576":   "78968d2bd8961c25395d6018fe49466671f0cdeeb33540f4cf1aa262d7ed2745",
	"zstd/2/text-4096":      "d46329bdd36cad4703ccdacc3caed09ac169ee251e1fef0880d4821de7d5e17e",
	"zstd/2/zeros-1048576":  "e4f5a0c53f8a6bda0af0e6cfcf4501ab9840ccd037f5dfa1d187720441b25744",
	"zstd/2/zeros-4096":     "3f6eb289782a5e7edc82aae9f37d424a6fe76171fc0ee720459c931071391e40",
	"zstd/4/noise-1048576":  "2ea0659ac52388fb7919c659eda3b8e330a56122f3f6bda01c293eb326b62737",
	"zstd/4/noise-4096":     "25fe655355d37dad452781e2857d7f721997bec9f127a5f2230d2865bf0412d3",
	"zstd/4/text-1048576":   "bbc1a6fca96a466e204a2df5e121673301094e5358d15d99470741d1a6f76cef",
	"zstd/4/text-4096":      "3c38179f3867592cb15c237e8efb03725fb45ffd2db5f0123c75bd2c74e74b92",
	"zstd/4/zeros-1048576":  "c86cf7bfca49b446d0d0806e0a2ecf1c069526345eb76ca63d6a28e93e30e880",
	"zstd/4/zeros-4096":     "3f6eb289782a5e7edc82aae9f37d424a6fe76171fc0ee720459c931071391e40",
}

// pinnedCompressVersion is the module version whose bytes goldenCompressed
// records. Bumping klauspost/compress must update it in the same change set,
// even when the output happens to stay byte-identical: the upgrade still has
// to be reviewed for the one-time --dedup reload documented in the CHANGELOG.
const pinnedCompressVersion = "v1.20.4"

// TestGoldenPinsMatchDependencyVersion closes the gap in the byte check: a
// library can change its encoder internals without changing these particular
// fixtures. Without this guard such a bump would go unnoticed by both tests,
// and the next --dedup run would silently re-upload under a new library.
func TestGoldenPinsMatchDependencyVersion(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info: cannot verify which compression library produced the golden pins")
	}
	for _, dep := range info.Deps {
		if dep.Path != "github.com/klauspost/compress" {
			continue
		}
		if dep.Version != pinnedCompressVersion {
			t.Fatalf("golden pins are for %s, build uses %s: update goldenCompressed, "+
				"pinnedCompressVersion and the CHANGELOG before shipping", pinnedCompressVersion, dep.Version)
		}
		return
	}
	t.Fatal("github.com/klauspost/compress missing from build info")
}
