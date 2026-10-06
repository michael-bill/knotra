package engine

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

func TestPayloadCodecLargeAggregateHasNoHidden256MiBCap(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_LARGE_PAYLOADS") != "1" {
		t.Skip("set KNOTRA_TEST_LARGE_PAYLOADS=1 for the 257 MiB aggregate payload regression")
	}
	store, err := NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codec := &PayloadCodec{Store: store}
	const size = 257 << 20
	payload := &commonpb.Payload{
		Metadata: map[string][]byte{converter.MetadataEncoding: []byte("binary/plain")},
		Data:     bytes.Repeat([]byte("x"), size),
	}
	encoded, err := codec.Encode([]*commonpb.Payload{payload})
	if err != nil {
		t.Fatal(err)
	}
	goruntime.GC()
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || len(decoded[0].Data) != size || decoded[0].Data[0] != 'x' || decoded[0].Data[size-1] != 'x' || string(decoded[0].Metadata[converter.MetadataEncoding]) != "binary/plain" {
		t.Fatal("large payload did not round trip")
	}
	t.Logf("round-tripped %d bytes through immutable storage and SHA256-verified reference", size)
}

func TestPayloadCodecRoundTripPreservesMetadata(t *testing.T) {
	store, err := NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codec := &PayloadCodec{Store: store, Threshold: 128}
	original := []*commonpb.Payload{
		{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte("json/plain")},
			Data:     []byte(`"small"`),
		},
		{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte("json/plain"), "custom": {0, 255}},
			Data:     bytes.Repeat([]byte("value"), 100000),
		},
	}
	encoded, err := codec.Encode(original)
	if err != nil {
		t.Fatal(err)
	}
	if encoded[0] != original[0] {
		t.Fatal("small payload should be unchanged")
	}
	if len(encoded[1].Data) != 64 || string(encoded[1].Metadata[converter.MetadataEncoding]) != externalPayloadEncoding {
		t.Fatal("large payload was not offloaded")
	}
	if string(original[1].Metadata[converter.MetadataEncoding]) != "json/plain" {
		t.Fatal("codec mutated input")
	}
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	for index := range original {
		if !proto.Equal(original[index], decoded[index]) {
			t.Fatalf("payload %d changed", index)
		}
	}
	// A new instance, as after process restart, reads the same persistent bytes.
	reopened, err := NewFileBlobStore(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&PayloadCodec{Store: reopened}).Decode(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadCodecRejectsMissingAndTamperedBlobs(t *testing.T) {
	for _, mode := range []string{"missing", "tampered", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			store, err := NewFileBlobStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			codec := &PayloadCodec{Store: store, Threshold: 1}
			encoded, err := codec.Encode([]*commonpb.Payload{{
				Metadata: map[string][]byte{converter.MetadataEncoding: []byte("json/plain")},
				Data:     []byte(`"value"`),
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.directory, string(encoded[0].Data))

			switch mode {
			case "missing":
				err = os.Remove(path)
			case "tampered":
				err = os.WriteFile(path, []byte("tampered"), 0600)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "payload")
				if err = os.Rename(path, outside); err == nil {
					err = os.Symlink(outside, path)
				}
			}

			if err != nil {
				t.Fatal(err)
			}
			if _, err := codec.Decode(encoded); err == nil {
				t.Fatal("corrupt blob was accepted")
			}
		})
	}
}

func TestPayloadCodecRejectsTraversalBeforeReading(t *testing.T) {
	store, err := NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codec := &PayloadCodec{Store: store}

	for _, key := range []string{"../secret", "", string(bytes.Repeat([]byte("A"), 64))} {
		payload := &commonpb.Payload{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte(externalPayloadEncoding)},
			Data:     []byte(key),
		}
		if _, err := codec.Decode([]*commonpb.Payload{payload}); err == nil {
			t.Fatalf("invalid key accepted: %q", key)
		}
	}
}

func TestBlobStoreConcurrentWritesAreIdempotent(t *testing.T) {
	store, err := NewFileBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("payload"), 10000)
	errors := make(chan error, 8)
	var group sync.WaitGroup

	for range 8 {
		group.Go(func() {
			key, err := store.Put(data)
			if err == nil {
				var got []byte
				got, err = store.Get(key)
				if err == nil && !bytes.Equal(got, data) {
					t.Error("content changed")
				}
			}
			errors <- err
		})
	}

	group.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}
