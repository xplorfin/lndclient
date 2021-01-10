package lndclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

var (
	rpcTimeout = 30 * time.Second

	// chainSyncPollInterval is the interval in which we poll the GetInfo
	// call to find out if lnd is fully synced to its chain backend.
	chainSyncPollInterval = 5 * time.Second

	// minimalCompatibleVersion is the minimum version and build tags
	// required in lnd to get all functionality implemented in lndclient.
	// Users can provide their own, specific version if needed. If only a
	// subset of the lndclient functionality is needed, the required build
	// tags can be adjusted accordingly. This default will be used as a fall
	// back version if none is specified in the configuration.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor:  0,
		AppMinor:  11,
		AppPatch:  0,
		BuildTags: DefaultBuildTags,
	}

	// ErrVersionCheckNotImplemented is the error that is returned if the
	// version RPC is not implemented in lnd. This means the version of lnd
	// is lower than v0.10.0-beta.
	ErrVersionCheckNotImplemented = errors.New("version check not " +
		"implemented, need minimum lnd version of v0.10.0-beta")

	// ErrVersionIncompatible is the error that is returned if the connected
	// lnd instance is not supported.
	ErrVersionIncompatible = errors.New("version incompatible")

	// ErrBuildTagsMissing is the error that is returned if the
	// connected lnd instance does not have all built tags activated that
	// are required.
	ErrBuildTagsMissing = errors.New("build tags missing")

	// DefaultBuildTags is the list of all subserver build tags that are
	// required for lndclient to work.
	DefaultBuildTags = []string{
		"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
	}
)

// LndServicesConfig holds all configuration settings that are needed to connect
// to an lnd node.
type LndServicesConfig struct {
	// LndAddress is the network address (host:port) of the lnd node to
	// connect to.
	LndAddress string

	// Network is the bitcoin network we expect the lnd node to operate on.
	Network Network

	// MacaroonDir is the directory where all lnd macaroons can be found.
	// One of this, CustomMacaroonPath, or CustomMacaroon can be specified,
	// but only one.
	MacaroonDir string

	// CustomMacaroonPath is the full path to a custom macaroon file.
	// One of this, MacaroonDir, or CustomMacaroon can be specified,
	// but only one.
	CustomMacaroonPath string

	// CustomMacaroon contains the raw data of any/all provided lnd macaroons.
	// One of this, MacaroonDir, or CustomMacaroonPath can be specified,
	// but only one.
	CustomMacaroon []byte

	// TLSPath is the path to lnd's TLS certificate file.
	TLSPath string

	// Raw byte data of lnd's TLS certificate file.
	RawTLS []byte

	// CheckVersion is the minimum version the connected lnd node needs to
	// be in order to be compatible. The node will be checked against this
	// when connecting. If no version is supplied, the default minimum
	// version will be used.
	CheckVersion *verrpc.Version

	// Dialer is an optional dial function that can be passed in if the
	// default lncfg.ClientAddressDialer should not be used.
	Dialer DialerFunc

	// BlockUntilChainSynced denotes that the NewLndServices function should
	// block until the lnd node is fully synced to its chain backend. This
	// can take a long time if lnd was offline for a while or if the initial
	// block download is still in progress.
	BlockUntilChainSynced bool

	// ChainSyncCtx is an optional context that can be passed in when
	// BlockUntilChainSynced is set to true. If a context is passed in and
	// its Done() channel sends a message, the wait for chain sync is
	// aborted. This allows a client to still be shut down properly if lnd
	// takes a long time to sync.
	ChainSyncCtx context.Context
}

// DialerFunc is a function that is used as grpc.WithContextDialer().
type DialerFunc func(context.Context, string) (net.Conn, error)

// availablePermissions contains any/all available permissions
// for clients and subclients. If a field is set to false,
// that client/subclient cannot be used.
type availablePermissions struct {
	lightning     bool
	walletKit     bool
	chainNotifier bool
	signer        bool
	invoices      bool
	router        bool
	readOnly      bool
}

// LndServices constitutes a set of required services.
type LndServices struct {
	Client        LightningClient
	WalletKit     WalletKitClient
	ChainNotifier ChainNotifierClient
	Signer        SignerClient
	Invoices      InvoicesClient
	Router        RouterClient
	Versioner     VersionerClient

	ChainParams *chaincfg.Params
	NodeAlias   string
	NodePubkey  [33]byte
	Version     *verrpc.Version

	macaroons *macaroonPouch

	permissions *availablePermissions
}

// GrpcLndServices constitutes a set of required RPC services.
type GrpcLndServices struct {
	LndServices

	cleanup func()
}

// NewLndServices creates creates a connection to the given lnd instance and
// creates a set of required RPC services.
func NewLndServices(cfg *LndServicesConfig) (*GrpcLndServices, error) {
	// We need to use a custom dialer so we can also connect to unix
	// sockets and not just TCP addresses.
	if cfg.Dialer == nil {
		cfg.Dialer = lncfg.ClientAddressDialer(defaultRPCPort)
	}

	// Fall back to minimal compatible version if none if specified.
	if cfg.CheckVersion == nil {
		cfg.CheckVersion = minimalCompatibleVersion
	}

	// We don't allow setting both the macaroon directory and the custom
	// macaroon path. If both are empty, that's fine, the default behavior
	// is to use lnd's default directory to try to locate the macaroons.
	if cfg.CustomMacaroon == nil && (cfg.MacaroonDir != "" && cfg.CustomMacaroonPath != "") {
		return nil, fmt.Errorf("if CustomMacaroon is not provided, " +
			"must set either MacaroonDir or " +
			"CustomMacaroonPath but not both")
	}

	var (
		macaroonDir   string
		loadMacDirErr error
	)

	if cfg.CustomMacaroon == nil {
		macaroonDir, loadMacDirErr = loadMacaroonsFromDirectory(cfg)
		if loadMacDirErr != nil {
			return nil, loadMacDirErr
		}
	}

	// Setup connection with lnd
	log.Infof("Creating lnd connection to %v", cfg.LndAddress)
	conn, err := getClientConn(cfg)
	if err != nil {
		return nil, err
	}

	log.Infof("Connected to lnd")

	chainParams, err := cfg.Network.ChainParams()
	if err != nil {
		return nil, err
	}

	// We are going to check that the connected lnd is on the same network
	// and is a compatible version with all the required subservers enabled.
	// For this, we make two calls, both of which only need the readonly
	// macaroon. We don't use the pouch yet because if not all subservers
	// are enabled, then not all macaroons might be there and the user would
	// get a more cryptic error message.
	var readonlyMac serializedMacaroon
	if cfg.CustomMacaroon == nil {
		var loadMacErr error

		readonlyMac, loadMacErr = loadMacaroon(
			macaroonDir, defaultReadonlyFilename, cfg.CustomMacaroonPath,
		)

		if loadMacErr != nil {
			return nil, loadMacErr
		}
	} else {
		readonlyMac = serializeBytesToMacaroon(cfg.CustomMacaroon)
	}

	// check that our provided macaroon(s) can perform the readonly
	// operations necessary for initializing the client
	if !checkMacaroonPermissions(readonlyMac, readOnlyRequiredPermssions) {
		return nil, fmt.Errorf("permissions needed for readonly operations " +
			"not found in provided macaroon(s)")
	}

	nodeAlias, nodeKey, version, err := checkLndCompatibility(
		conn, chainParams, readonlyMac, cfg.Network, cfg.CheckVersion,
	)
	if err != nil {
		return nil, err
	}

	// Now that we've ensured our macaroon directory is set properly, we
	// can retrieve our full macaroon pouch from the directory.
	macaroons, loadMacPouchErr := newMacaroonPouch(macaroonDir, cfg.CustomMacaroonPath, cfg.CustomMacaroon)
	if loadMacPouchErr != nil {
		return nil, fmt.Errorf("unable to obtain macaroons: %v", loadMacPouchErr)
	}

	// Check which clients our macaroon(s) can access
	// and add those clients to lndServices accordingly
	permissions := loadAvailablePermissions(macaroons)
	var cleanupFuncs []func()

	var lndServices = LndServices{
		ChainParams: chainParams,
		NodeAlias:   nodeAlias,
		NodePubkey:  nodeKey,
		Version:     version,
		macaroons:   macaroons,
		permissions: permissions,
	}

	// With the macaroons loaded and the version checked, we can now create
	// the real lightning client which uses the admin macaroon.
	if permissions.lightning {
		lightningClient := newLightningClient(conn, chainParams, macaroons.adminMac)
		lndServices.Client = lightningClient

		cleanupFuncs = append(cleanupFuncs, func() {
			log.Debugf("Wait for client to shut down")
			lightningClient.WaitForFinished()
		})
	} else {
		return nil, fmt.Errorf("required permissions for main lightning client " +
			"not available, please use a different macaroon")
	}

	// With the network check passed, we'll now initialize the rest of the
	// sub-server connections, giving each of them their specific macaroon.
	lndServices.Versioner = newVersionerClient(conn, macaroons.readonlyMac)

	if permissions.chainNotifier {
		notifierClient := newChainNotifierClient(conn, macaroons.chainMac)
		lndServices.ChainNotifier = notifierClient

		cleanupFuncs = append(cleanupFuncs, func() {
			log.Debugf("Wait for chain notifier client to shut down")
			notifierClient.WaitForFinished()
		})
	}

	if permissions.invoices {
		invoicesClient := newInvoicesClient(conn, macaroons.invoiceMac)
		lndServices.Invoices = invoicesClient

		cleanupFuncs = append(cleanupFuncs, func() {
			log.Debugf("Wait for invoices client to shut down")
			invoicesClient.WaitForFinished()
		})
	}

	if permissions.signer {
		lndServices.Signer = newSignerClient(conn, macaroons.signerMac)
	}

	if permissions.walletKit {
		lndServices.WalletKit = newWalletKitClient(conn, macaroons.walletKitMac)
	}

	if permissions.router {
		lndServices.Router = newRouterClient(conn, macaroons.routerMac)
	}

	cleanup := func() {
		log.Debugf("Closing lnd connection")

		if err := conn.Close(); err != nil {
			log.Errorf("Error closing client connection: %v", err)
		}

		for _, cleanupFunc := range cleanupFuncs {
			cleanupFunc()
		}

		log.Debugf("Lnd services finished")
	}

	services := &GrpcLndServices{
		LndServices: lndServices,
		cleanup:     cleanup,
	}

	log.Infof("Using network %v", cfg.Network)

	// If requested in the configuration, we now wait for lnd to fully sync
	// to its chain backend. We do not add any timeout as it would be hard
	// to determine a sane value. If the initial block download is still in
	// progress, this could take hours.
	if cfg.BlockUntilChainSynced {
		log.Infof("Waiting for lnd to be fully synced to its chain " +
			"backend, this might take a while")

		err := services.waitForChainSync(cfg.ChainSyncCtx)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("error waiting for chain to "+
				"be synced: %v", err)
		}

		log.Infof("lnd is now fully synced to its chain backend")
	}

	return services, nil
}

// Close closes the lnd connection and waits for all sub server clients to
// finish their goroutines.
func (s *GrpcLndServices) Close() {
	s.cleanup()

	log.Debugf("Lnd services finished")
}

// GetAvailableClients returns a string slice containing
// the names of all available clients, based the permissions found
// in any loaded macaroons.
func (s *GrpcLndServices) GetAvailableClients() []string {
	var availablePerms []string

	if s.permissions.lightning {
		availablePerms = append(availablePerms, "lightning")
	}

	if s.permissions.walletKit {
		availablePerms = append(availablePerms, "walletkit")
	}

	if s.permissions.router {
		availablePerms = append(availablePerms, "router")
	}

	if s.permissions.signer {
		availablePerms = append(availablePerms, "signer")
	}

	if s.permissions.invoices {
		availablePerms = append(availablePerms, "invoices")
	}

	if s.permissions.chainNotifier {
		availablePerms = append(availablePerms, "chainnotifier")
	}

	if s.permissions.readOnly {
		availablePerms = append(availablePerms, "readonly")
	}

	return availablePerms
}

// waitForChainSync waits and blocks until the connected lnd node is fully
// synced to its chain backend. This could theoretically take hours if the
// initial block download is still in progress.
func (s *GrpcLndServices) waitForChainSync(ctx context.Context) error {
	mainCtx := ctx
	if mainCtx == nil {
		mainCtx = context.Background()
	}

	// We spawn a goroutine that polls in regular intervals and reports back
	// once the chain is ready (or something went wrong). If the chain is
	// already synced, this should return almost immediately.
	update := make(chan error)
	go func() {
		for {
			// The GetInfo call can take a while. But if it takes
			// too long, that can be a sign of something being wrong
			// with the node. That's why we don't wait any longer
			// than a few seconds for each individual GetInfo call.
			ctxt, cancel := context.WithTimeout(mainCtx, rpcTimeout)
			info, err := s.Client.GetInfo(ctxt)
			if err != nil {
				cancel()
				update <- fmt.Errorf("error in GetInfo call: "+
					"%v", err)
				return
			}
			cancel()

			// We're done, deliver a nil update by closing the chan.
			if info.SyncedToChain {
				close(update)
				return
			}

			select {
			// If we're not yet done, let's now wait a few seconds.
			case <-time.After(chainSyncPollInterval):

			// If the user cancels the context, we should also
			// abort the wait.
			case <-mainCtx.Done():
				update <- mainCtx.Err()
				return
			}
		}
	}()

	// Wait for either an error or the nil close signal to arrive.
	return <-update
}

// If loading macaroons from a specific directory,
// loadMacaroonsFromDirectory creates a fully qualified
// path to the macaroons directory based on the network.
func loadMacaroonsFromDirectory(cfg *LndServicesConfig) (string, error) {
	// Based on the network, if the macaroon directory isn't set, then
	// we'll use the expected default locations.
	macaroonDir := cfg.MacaroonDir
	if macaroonDir == "" {
		switch cfg.Network {
		case NetworkTestnet:
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "testnet",
			)

		case NetworkMainnet:
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "mainnet",
			)

		case NetworkSimnet:
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "simnet",
			)

		case NetworkRegtest:
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "regtest",
			)

		default:
			return "", fmt.Errorf("unsupported network: %v",
				cfg.Network)
		}
	}

	return macaroonDir, nil
}

// checkLndCompatibility makes sure the connected lnd instance is running on the
// correct network, has the version RPC implemented, is the correct minimal
// version and supports all required build tags/subservers.
func checkLndCompatibility(conn *grpc.ClientConn, chainParams *chaincfg.Params,
	readonlyMac serializedMacaroon, network Network,
	minVersion *verrpc.Version) (string, [33]byte, *verrpc.Version, error) {

	// onErr is a closure that simplifies returning multiple values in the
	// error case.
	onErr := func(err error) (string, [33]byte, *verrpc.Version, error) {
		closeErr := conn.Close()
		if closeErr != nil {
			log.Errorf("Error closing lnd connection: %v", closeErr)
		}

		// Make static error messages a bit less cryptic by adding the
		// version or build tag that we expect.
		newErr := fmt.Errorf("lnd compatibility check failed: %v", err)
		if err == ErrVersionIncompatible || err == ErrBuildTagsMissing {
			newErr = fmt.Errorf("error checking connected lnd "+
				"version. at least version \"%s\" is "+
				"required", VersionString(minVersion))
		}

		return "", [33]byte{}, nil, newErr
	}

	// We use our own clients with a readonly macaroon here, because we know
	// that's all we need for the checks.
	lightningClient := newLightningClient(conn, chainParams, readonlyMac)
	versionerClient := newVersionerClient(conn, readonlyMac)

	// With our readonly macaroon obtained, we'll ensure that the network
	// for lnd matches our expected network.
	info, err := lightningClient.GetInfo(context.Background())
	if err != nil {
		err := fmt.Errorf("unable to get info for lnd node: %v", err)
		return onErr(err)
	}
	if string(network) != info.Network {
		err := fmt.Errorf("network mismatch with connected lnd node, "+
			"wanted '%s', got '%s'", network, info.Network)
		return onErr(err)
	}

	// Now let's also check the version of the connected lnd node.
	version, err := checkVersionCompatibility(versionerClient, minVersion)
	if err != nil {
		return onErr(err)
	}

	// Return the static part of the info we just queried from the node so
	// it can be cached for later use.
	return info.Alias, info.IdentityPubkey, version, nil
}

// checkVersionCompatibility makes sure the connected lnd node has the correct
// version and required build tags enabled.
//
// NOTE: This check will **never** return a non-nil error for a version of
// lnd < 0.10.0 because any version previous to 0.10.0 doesn't have the version
// endpoint implemented!
func checkVersionCompatibility(client VersionerClient,
	expected *verrpc.Version) (*verrpc.Version, error) {

	// First, test that the version RPC is even implemented.
	version, err := client.GetVersion(context.Background())
	if err != nil {
		// The version service has only been added in lnd v0.10.0. If
		// we get an unimplemented error, it means the lnd version is
		// definitely older than that.
		s, ok := status.FromError(err)
		if ok && s.Code() == codes.Unimplemented {
			return nil, ErrVersionCheckNotImplemented
		}
		return nil, fmt.Errorf("GetVersion error: %v", err)
	}

	log.Infof("lnd version: %v", VersionString(version))

	// Now check the version and make sure all required build tags are set.
	err = assertVersionCompatible(version, expected)
	if err != nil {
		return nil, err
	}
	err = assertBuildTagsEnabled(version, expected.BuildTags)
	if err != nil {
		return nil, err
	}

	// All check positive, version is fully compatible.
	return version, nil
}

// assertVersionCompatible makes sure the detected lnd version is compatible
// with our current version requirements.
func assertVersionCompatible(actual *verrpc.Version,
	expected *verrpc.Version) error {

	// We need to check the versions parts sequentially as they are
	// hierarchical.
	if actual.AppMajor != expected.AppMajor {
		if actual.AppMajor > expected.AppMajor {
			return nil
		}
		return ErrVersionIncompatible
	}

	if actual.AppMinor != expected.AppMinor {
		if actual.AppMinor > expected.AppMinor {
			return nil
		}
		return ErrVersionIncompatible
	}

	if actual.AppPatch != expected.AppPatch {
		if actual.AppPatch > expected.AppPatch {
			return nil
		}
		return ErrVersionIncompatible
	}

	// The actual version and expected version are identical.
	return nil
}

// assertBuildTagsEnabled makes sure all required build tags are set.
func assertBuildTagsEnabled(actual *verrpc.Version,
	requiredTags []string) error {

	tagMap := make(map[string]struct{})
	for _, tag := range actual.BuildTags {
		tagMap[tag] = struct{}{}
	}
	for _, required := range requiredTags {
		if _, ok := tagMap[required]; !ok {
			return ErrBuildTagsMissing
		}
	}

	// All tags found.
	return nil
}

var (
	defaultRPCPort         = "10009"
	defaultLndDir          = btcutil.AppDataDir("lnd", false)
	defaultTLSCertFilename = "tls.cert"
	defaultTLSCertPath     = filepath.Join(
		defaultLndDir, defaultTLSCertFilename,
	)
	defaultDataDir     = "data"
	defaultChainSubDir = "chain"

	defaultAdminMacaroonFilename     = "admin.macaroon"
	defaultInvoiceMacaroonFilename   = "invoices.macaroon"
	defaultChainMacaroonFilename     = "chainnotifier.macaroon"
	defaultWalletKitMacaroonFilename = "walletkit.macaroon"
	defaultRouterMacaroonFilename    = "router.macaroon"
	defaultSignerFilename            = "signer.macaroon"
	defaultReadonlyFilename          = "readonly.macaroon"

	// maxMsgRecvSize is the largest gRPC message our client will receive.
	// We set this to 200MiB.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
)

func getClientConn(cfg *LndServicesConfig) (*grpc.ClientConn, error) {
	var (
		creds          credentials.TransportCredentials
		loadCredsError error
	)

	switch {
	case cfg.RawTLS != nil:
		creds, loadCredsError = loadRawTls(cfg)
	default:
		creds, loadCredsError = loadTlsFromFile(cfg)
	}

	if loadCredsError != nil {
		return nil, fmt.Errorf("unable to load tls credentials: %v",
			loadCredsError)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),

		// Use a custom dialer, to allow connections to unix sockets,
		// in-memory listeners etc, and not just TCP addresses.
		grpc.WithContextDialer(cfg.Dialer),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	conn, err := grpc.Dial(cfg.LndAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

func loadTlsFromFile(cfg *LndServicesConfig) (credentials.TransportCredentials, error) {
	// Load the specified TLS certificate and build transport credentials
	// with it.
	tlsPath := cfg.TLSPath
	if tlsPath == "" {
		tlsPath = defaultTLSCertPath
	}

	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, err
	}

	return creds, nil
}

func loadRawTls(cfg *LndServicesConfig) (credentials.TransportCredentials, error) {
	tlsBytes := cfg.RawTLS

	certPool := x509.NewCertPool()

	if !certPool.AppendCertsFromPEM(tlsBytes) {
		return nil, fmt.Errorf("could not append raw tls cert to " +
			"x509 certpool")
	}

	creds := credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: false,
		RootCAs:            certPool,
	})

	return creds, nil
}
