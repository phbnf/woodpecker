package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mhutchinson/woodpecker/model"
	distclient "github.com/transparency-dev/distributor/client"
	"github.com/transparency-dev/formats/log"
	tnote "github.com/transparency-dev/formats/note"
	serverless_client "github.com/transparency-dev/serverless-log/client"
	tiles_client "github.com/transparency-dev/trillian-tessera/client"
	"golang.org/x/mod/sumdb/note"
	"k8s.io/klog/v2"
)

const distURL = "https://api.transparency.dev"

var (
	clients = []logClient{
		newServerlessLogClient("https://api.transparency.dev/armored-witness-firmware/prod/log/1/",
			"transparency.dev/armored-witness/firmware_transparency/prod/1",
			"transparency.dev-aw-ftlog-prod-1+3e6d87ee+Aa3qdhefd2cc/98jV3blslJT2L+iFR8WKHeGcgFmyjnt"),
		newServerlessLogClient("https://api.transparency.dev/armored-witness-firmware/ci/log/4/",
			"transparency.dev/armored-witness/firmware_transparency/ci/4",
			"transparency.dev-aw-ftlog-ci-4+30fe79e3+AUDoas+smwQDTlYbTzbEcAW+N6WyvB/4CysMWjpnRgat"),
		newServerlessLogClient("https://raw.githubusercontent.com/f-secure-foundry/armory-drive-log/master/log/",
			"Armory Drive Prod 2",
			"armory-drive-log+16541b8f+AYDPmG5pQp4Bgu0a1mr5uDZ196+t8lIVIfWQSPWmP+Jv"),
		newServerlessLogClient("https://fwupd.org/ftlog/lvfs/",
			"lvfs",
			"lvfs+7908d142+ASnlGgOh+634tcE/2Lp3wV7k/cLoU6ncawmb/BLC1oMU"),
		newSumDBLogClient("https://sum.golang.org/",
			"go.sum database tree",
			"sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"),
	}
)

var (
	customLogUrl    = flag.String("custom_log_url", "", "The base URL of a custom log to register")
	customLogOrigin = flag.String("custom_log_origin", "", "The origin of a custom log to register")
	customLogVKey   = flag.String("custom_log_vkey", "", "The verifier key of a custom log to register")
	customLogType   = flag.String("custom_log_type", "", "The type of the custom log specified by the other custom_* flags. Must be empty, or one of {tiles, serverless}.")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	switch *customLogType {
	case "":
		break
	case "tiles":
		c := newTLogTilesLogClient(*customLogUrl, *customLogOrigin, *customLogVKey)
		clients = append([]logClient{c}, clients...)
	case "serverless":
		c := newServerlessLogClient(*customLogUrl, *customLogOrigin, *customLogVKey)
		clients = append([]logClient{c}, clients...)
	default:
		klog.Exitf("custom_log_type %s not recognised", *customLogType)
	}
	logClients := make(map[string]logClient, len(clients))
	logOrigins := make([]string, 0, len(clients))
	for _, c := range clients {
		logClients[c.GetOrigin()] = c
		logOrigins = append(logOrigins, c.GetOrigin())
	}
	model := model.NewViewModel(logOrigins)
	controller := NewController(model, logClients, *distclient.NewRestDistributor(distURL, http.DefaultClient))
	controller.SelectLog(clients[0].GetOrigin())
	go func() {
		t := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				controller.RefreshCheckpoint()
			}
		}
	}()
	view := NewView(controller, model)
	if err := view.Run(context.Background()); err != nil {
		panic(err)
	}
}

func NewController(model *model.ViewModel, logClients map[string]logClient, distributor distclient.RestDistributor) *Controller {
	witKeys, err := distributor.GetWitnesses()
	if err != nil {
		panic(fmt.Sprintf("Witnesses not available: %v", err))
	}
	witVerifiers := make([]note.Verifier, 0, len(witKeys))
	for _, k := range witKeys {
		v, err := tnote.NewVerifierForCosignatureV1(k)
		if err != nil {
			panic(fmt.Sprintf("Invalid witness key: %v", err))
		}
		witVerifiers = append(witVerifiers, v)
	}
	return &Controller{
		Model:        model,
		LogClients:   logClients,
		Distributor:  distributor,
		witVerifiers: witVerifiers,
	}
}

type Controller struct {
	Model        *model.ViewModel
	LogClients   map[string]logClient
	Distributor  distclient.RestDistributor
	witVerifiers []note.Verifier

	current logClient
}

func (c *Controller) SelectLog(o string) {
	if n, ok := c.LogClients[o]; ok {
		c.current = n
		c.InitFromLog()
	}
}

func (c *Controller) InitFromLog() {
	c.RefreshCheckpoint()
	if err := c.Model.GetError(); err != nil {
		klog.Exit(err)
	}
	if cp := c.Model.GetCheckpoint(); cp != nil && cp.Size > 0 {
		size := cp.Size
		c.GetLeaf(size, size-1)
	}
}

func (c *Controller) RefreshCheckpoint() {
	witnessed := make(chan *model.Checkpoint)
	// Fetch the witnessed checkpoint in parallel
	go func() {
		logID := distclient.LogID(log.ID(c.current.GetOrigin()))
		bs, err := c.Distributor.GetCheckpointN(logID, c.Model.GetWitnessN())
		if err != nil {
			witnessed <- nil
			return
		}
		cp, _, n, _ := log.ParseCheckpoint(bs, c.current.GetOrigin(), c.current.GetVerifier(), c.witVerifiers...)
		witnessed <- &model.Checkpoint{
			Checkpoint: cp,
			Note:       n,
			Raw:        bs,
		}
	}()
	cp, err := c.current.GetCheckpoint()
	wCP := <-witnessed
	c.Model.SetCheckpoint(cp, wCP, err)
}

func (c *Controller) GetLeaf(size, index uint64) {
	if index >= size {
		c.Model.SetLeaf(c.Model.GetLeaf(), fmt.Errorf("Cannot fetch leaf bigger than checkpoint size %d", size))
		return
	}
	leaf, err := c.current.GetLeaf(size, index)
	c.Model.SetLeaf(model.Leaf{
		Contents: leaf,
		Index:    index,
	}, err)
}

func (c *Controller) PrevLeaf() {
	size := c.Model.GetCheckpoint().Size
	c.GetLeaf(size, c.Model.GetLeaf().Index-1)
}

func (c *Controller) NextLeaf() {
	size := c.Model.GetCheckpoint().Size
	c.GetLeaf(size, c.Model.GetLeaf().Index+1)
}

func (c *Controller) IncWitnesses() {
	c.Model.SetWitnessN(c.Model.GetWitnessN() + 1)
	c.RefreshCheckpoint()
}

func (c *Controller) DecWitnesses() {
	if c.Model.GetWitnessN() <= 1 {
		return
	}
	c.Model.SetWitnessN(c.Model.GetWitnessN() - 1)
	c.RefreshCheckpoint()
}

type logClient interface {
	GetOrigin() string
	GetVerifier() note.Verifier
	GetCheckpoint() (*model.Checkpoint, error)
	GetLeaf(size, index uint64) ([]byte, error)
}

func newTLogTilesLogClient(lr string, origin string, vkey string) logClient {
	if !strings.HasSuffix(lr, "/") {
		lr = lr + "/"
	}
	logRoot, err := url.Parse(lr)
	if err != nil {
		klog.Exitf("Failed to create URL from %q: %v", lr, err)
	}
	fetcher := newFetcher(logRoot)
	verifier, err := note.NewVerifier(vkey)
	if err != nil {
		klog.Exitf("Failed to create verifier from %q: %v", vkey, err)
	}
	if len(origin) == 0 {
		origin = verifier.Name()
		klog.Infof("No origin provided; using verifier name: %q", origin)
	}
	return &tLogTilesLogClient{
		origin:   origin,
		verifier: verifier,
		fetcher: func(ctx context.Context, path string) ([]byte, error) {
			return fetcher(ctx, path)
		},
	}
}

type tLogTilesLogClient struct {
	origin   string
	verifier note.Verifier
	fetcher  tiles_client.Fetcher
}

func (c *tLogTilesLogClient) GetOrigin() string {
	return c.origin
}

func (c *tLogTilesLogClient) GetVerifier() note.Verifier {
	return c.verifier
}

func (c *tLogTilesLogClient) GetCheckpoint() (*model.Checkpoint, error) {
	cp, raw, n, err := tiles_client.FetchCheckpoint(context.Background(), c.fetcher, c.verifier, c.origin)
	return &model.Checkpoint{
		Checkpoint: cp,
		Raw:        raw,
		Note:       n,
	}, err
}

func (c *tLogTilesLogClient) GetLeaf(size, index uint64) ([]byte, error) {
	bundleIndex := index / 256
	leafOffset := index % 256
	// TODO(mhutchinson): cache the bundle so consecutive leaf fetching is efficient
	bundle, err := tiles_client.GetEntryBundle(context.Background(), c.fetcher, bundleIndex, size)
	if err != nil {
		return nil, err
	}
	return bundle.Entries[leafOffset], nil
}

func newServerlessLogClient(lr string, origin string, vkey string) logClient {
	if !strings.HasSuffix(lr, "/") {
		lr = lr + "/"
	}
	logRoot, err := url.Parse(lr)
	if err != nil {
		klog.Exit(err)
	}
	fetcher := newFetcher(logRoot)
	verifier, err := note.NewVerifier(vkey)
	if err != nil {
		klog.Exit(err)
	}
	return &serverlessLogClient{
		origin:   origin,
		verifier: verifier,
		fetcher:  fetcher,
	}
}

type serverlessLogClient struct {
	origin   string
	verifier note.Verifier
	fetcher  serverless_client.Fetcher
}

func (c *serverlessLogClient) GetOrigin() string {
	return c.origin
}

func (c *serverlessLogClient) GetVerifier() note.Verifier {
	return c.verifier
}

func (c *serverlessLogClient) GetCheckpoint() (*model.Checkpoint, error) {
	cp, raw, n, err := serverless_client.FetchCheckpoint(context.Background(), c.fetcher, c.verifier, c.origin)
	return &model.Checkpoint{
		Checkpoint: cp,
		Raw:        raw,
		Note:       n,
	}, err
}

func (c *serverlessLogClient) GetLeaf(size, index uint64) ([]byte, error) {
	leaf, err := serverless_client.GetLeaf(context.Background(), c.fetcher, index)
	return leaf, err
}

func newSumDBLogClient(lr string, origin string, vkey string) logClient {
	logRoot, err := url.Parse(lr)
	if err != nil {
		klog.Exit(err)
	}
	fetcher := newFetcher(logRoot)
	verifier, err := note.NewVerifier(vkey)
	if err != nil {
		klog.Exit(err)
	}
	return &sumDBLogClient{
		origin:   origin,
		verifier: verifier,
		fetcher:  fetcher,
	}
}

type sumDBLogClient struct {
	origin   string
	verifier note.Verifier
	fetcher  serverless_client.Fetcher
}

func (c *sumDBLogClient) GetOrigin() string {
	return c.origin
}

func (c *sumDBLogClient) GetVerifier() note.Verifier {
	return c.verifier
}

func (c *sumDBLogClient) GetCheckpoint() (*model.Checkpoint, error) {
	cpRaw, err := c.fetcher(context.Background(), "/latest")
	if err != nil {
		return nil, err
	}

	cp, _, n, err := log.ParseCheckpoint(cpRaw, c.origin, c.verifier)
	return &model.Checkpoint{
		Checkpoint: cp,
		Raw:        cpRaw,
		Note:       n,
	}, err
}

func (c *sumDBLogClient) GetLeaf(size, index uint64) ([]byte, error) {
	const pathBase = 1000
	offset := index / 256
	nStr := fmt.Sprintf("%03d", offset%pathBase)
	for offset >= pathBase {
		offset /= pathBase
		nStr = fmt.Sprintf("x%03d/%s", offset%pathBase, nStr)
	}
	path := fmt.Sprintf("/tile/8/data/%s", nStr)
	if rem := index % 256; rem != 255 {
		path = fmt.Sprintf("%s.p/%d", path, rem+1)
	}
	data, err := c.fetcher(context.Background(), path)
	if err != nil {
		return nil, err
	}
	dataToLeaves := func(data []byte) [][]byte {
		result := make([][]byte, 0)
		start := 0
		for i, b := range data {
			if b == '\n' {
				if i > start && data[i-1] == '\n' {
					result = append(result, data[start:i])
					start = i + 1
				}
			}
		}
		result = append(result, data[start:])
		return result
	}
	leaves := dataToLeaves(data)
	return leaves[index%256], nil
}

// newFetcher creates a Fetcher for the log at the given root location.
func newFetcher(root *url.URL) serverless_client.Fetcher {
	get := getByScheme[root.Scheme]
	if get == nil {
		panic(fmt.Errorf("unsupported URL scheme %s", root.Scheme))
	}

	return func(ctx context.Context, p string) ([]byte, error) {
		u, err := root.Parse(p)
		if err != nil {
			return nil, err
		}
		return get(ctx, u)
	}
}

var getByScheme = map[string]func(context.Context, *url.URL) ([]byte, error){
	"http":  readHTTP,
	"https": readHTTP,
	"file": func(_ context.Context, u *url.URL) ([]byte, error) {
		return os.ReadFile(u.Path)
	},
}

func readHTTP(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case 404:
		klog.Infof("Not found: %q", u.String())
		return nil, os.ErrNotExist
	case 200:
		break
	default:
		return nil, fmt.Errorf("unexpected http status %q", resp.Status)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Errorf("resp.Body.Close(): %v", err)
		}
	}()
	return io.ReadAll(resp.Body)
}
