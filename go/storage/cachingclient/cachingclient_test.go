package cachingclient

import (
	"context"
	"crypto"
	"crypto/rand"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/oasislabs/ekiden/go/common/crypto/drbg"
	"github.com/oasislabs/ekiden/go/common/crypto/hash"
	"github.com/oasislabs/ekiden/go/common/crypto/signature"
	"github.com/oasislabs/ekiden/go/storage/api"
	"github.com/oasislabs/ekiden/go/storage/memory"
	"github.com/oasislabs/ekiden/go/storage/tests"
)

const cacheSize = 10

func TestCachingClient(t *testing.T) {
	var err error

	var sk signature.PrivateKey
	sk, err = signature.NewPrivateKey(rand.Reader)
	require.NoError(t, err, "failed to generate dummy receipt signing key")
	remote := memory.New(&sk)
	client, cacheDir := requireNewClient(t, remote)
	defer func() {
		os.RemoveAll(cacheDir)
	}()

	wl := makeTestWriteLog([]byte("TestSingle"), cacheSize)
	expectedNewRoot := tests.CalculateExpectedNewRoot(t, wl)

	var root hash.Hash
	root.Empty()
	receipts, err := client.Apply(context.Background(), root, expectedNewRoot, wl)
	require.NoError(t, err, "Apply() should not return an error")
	require.NotNil(t, receipts, "Apply() should return receipts")

	// Check the receipts and ensure they contain a new root that equals the
	// expected new root.
	var receiptBody api.ReceiptBody
	for _, receipt := range receipts {
		err = receipt.Open(&receiptBody)
		require.NoError(t, err, "receipt.Open() should not return an error")
		require.Equal(t, 1, len(receiptBody.Roots), "receiptBody should contain 1 root")
		require.EqualValues(t, expectedNewRoot, receiptBody.Roots[0], "receiptBody root should equal the expected new root")
	}

	// Check if the values match.
	for i, kv := range wl {
		var h hash.Hash
		h.FromBytes(kv.Value)
		v, e := client.GetValue(context.Background(), expectedNewRoot, h)
		require.NoError(t, e, "Get1")
		require.EqualValues(t, kv.Value, v, "Get1 - value: %d", i)
	}

	// Test the persistence.
	client.Cleanup()
	remote = memory.New(&sk)
	_, err = New(remote)
	require.NoError(t, err, "New - reopen")

	// Check if the values are still fetchable.
	for i, kv := range wl {
		var h hash.Hash
		h.FromBytes(kv.Value)
		v, e := client.GetValue(context.Background(), expectedNewRoot, h)
		require.NoError(t, e, "Get2")
		require.EqualValues(t, kv.Value, v, "Get2 - value: %d", i)
	}
}

func requireNewClient(t *testing.T, remote api.Backend) (api.Backend, string) {
	<-remote.Initialized()
	cacheDir, err := ioutil.TempDir("", "ekiden-cachingclient-test_")
	require.NoError(t, err, "create cache dir")

	viper.Set(cfgCacheFile, filepath.Join(cacheDir, "db"))
	viper.Set(cfgCacheSize, 1024768)

	client, err := New(remote)
	if err != nil {
		os.RemoveAll(cacheDir)
	}
	require.NoError(t, err, "New")

	return client, cacheDir
}

func makeTestWriteLog(seed []byte, n int) api.WriteLog {
	h := crypto.SHA512.New()
	_, _ = h.Write(seed)
	drbg, err := drbg.New(crypto.SHA256, h.Sum(nil), nil, seed)
	if err != nil {
		panic(err)
	}

	var wl api.WriteLog
	for i := 0; i < n; i++ {
		v := make([]byte, 64)
		_, _ = drbg.Read(v)
		wl = append(wl, api.LogEntry{
			Key:   []byte(strconv.Itoa(i)),
			Value: v,
		})
	}

	return wl
}
