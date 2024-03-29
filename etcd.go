// Package etcd provides an etcd service register
package etcd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	hash "github.com/mitchellh/hashstructure"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"go.unistack.org/micro/v3/logger"
	"go.unistack.org/micro/v3/register"
)

var DefaultPrefix = "/micro/register/"

var _ register.Register = &etcdRegister{}

type etcdRegister struct {
	client  *clientv3.Client
	options register.Options

	// register and leases are grouped by domain
	sync.RWMutex
	reg    map[string]reg
	leases map[string]leases
}

type (
	reg    map[string]uint64
	leases map[string]clientv3.LeaseID
)

// NewRegister returns an initialized etcd register
func NewRegister(opts ...register.Option) *etcdRegister {
	e := &etcdRegister{
		options: register.NewOptions(opts...),
		reg:     make(map[string]reg),
		leases:  make(map[string]leases),
	}
	configure(e, opts...)
	return e
}

func newClient(e *etcdRegister) (*clientv3.Client, error) {
	config := clientv3.Config{
		Endpoints: []string{"127.0.0.1:2379"},
	}

	if e.options.Timeout == 0 {
		e.options.Timeout = 5 * time.Second
	}

	if e.options.TLSConfig != nil {
		tlsConfig := e.options.TLSConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}

		config.TLS = tlsConfig
	}

	if e.options.Context != nil {
		u, ok := e.options.Context.Value(authKey{}).(*authCreds)
		if ok {
			config.Username = u.Username
			config.Password = u.Password
		}
		cfg, ok := e.options.Context.Value(logConfigKey{}).(*zap.Config)
		if ok && cfg != nil {
			config.LogConfig = cfg
		}
	}

	var cAddrs []string

	for _, address := range e.options.Addrs {
		if len(address) == 0 {
			continue
		}
		addr, port, err := net.SplitHostPort(address)
		if ae, ok := err.(*net.AddrError); ok && ae.Err == "missing port in address" {
			port = "2379"
			addr = address
			cAddrs = append(cAddrs, net.JoinHostPort(addr, port))
		} else if err == nil {
			cAddrs = append(cAddrs, net.JoinHostPort(addr, port))
		}
	}

	// if we got addrs then we'll update
	if len(cAddrs) > 0 {
		config.Endpoints = cAddrs
	}

	// check if the endpoints have https://
	if config.TLS != nil {
		for i, ep := range config.Endpoints {
			if !strings.HasPrefix(ep, "https://") {
				config.Endpoints[i] = "https://" + ep
			}
		}
	}

	cli, err := clientv3.New(config)
	if err != nil {
		return nil, err
	}

	return cli, nil
}

// configure will setup the registry with new options
func configure(e *etcdRegister, opts ...register.Option) error {
	for _, o := range opts {
		o(&e.options)
	}

	// setup the client
	cli, err := newClient(e)
	if err != nil {
		return err
	}

	if e.client != nil {
		e.client.Close()
	}

	// setup new client
	e.client = cli

	return nil
}

// getName returns the domain and name
// it returns false if there's an issue
// the key is a path of /prefix/domain/name/id e.g /micro/registry/domain/service/uuid
func getName(key, prefix string) (string, string, bool) {
	// strip the prefix from keys
	key = strings.TrimPrefix(key, prefix)

	// split the key so we remove domain
	parts := strings.Split(key, "/")

	if len(parts) == 0 {
		return "", "", false
	}

	if len(parts[0]) == 0 {
		parts = parts[1:]
	}

	// we expect a domain and then name domain/service
	if len(parts) < 2 {
		return "", "", false
	}

	// return name, domain
	return parts[0], parts[1], true
}

func encode(s *register.Service) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func decode(ds []byte) *register.Service {
	var s *register.Service
	json.Unmarshal(ds, &s)
	return s
}

func nodePath(domain, s, id string) string {
	service := strings.Replace(s, "/", "-", -1)
	node := strings.Replace(id, "/", "-", -1)
	return path.Join(prefixWithDomain(domain), service, node)
}

func servicePath(domain, s string) string {
	return path.Join(prefixWithDomain(domain), serializeServiceName(s))
}

func serializeServiceName(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}

func prefixWithDomain(domain string) string {
	return path.Join(DefaultPrefix, domain)
}

func (e *etcdRegister) Init(opts ...register.Option) error {
	return configure(e, opts...)
}

func (e *etcdRegister) Options() register.Options {
	return e.options
}

func (e *etcdRegister) Connect(ctx context.Context) error {
	// TODO: real connect to etcd
	return nil
}

func (e *etcdRegister) Disconnect(ctx context.Context) error {
	// TODO: real diconnect from etcd
	return nil
}

func (e *etcdRegister) registerNode(s *register.Service, node *register.Node, opts ...register.RegisterOption) error {
	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	// parse the options
	options := register.NewRegisterOptions(opts...)

	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	if node.Metadata == nil {
		node.Metadata = map[string]string{}
	}

	// set the domain in metadata so it can be retrieved by wildcard queries
	s.Metadata["domain"] = options.Domain
	node.Metadata["domain"] = options.Domain

	e.Lock()
	// ensure the leases and registers are setup for this domain
	if _, ok := e.leases[options.Domain]; !ok {
		e.leases[options.Domain] = make(leases)
	}
	if _, ok := e.reg[options.Domain]; !ok {
		e.reg[options.Domain] = make(reg)
	}

	// check to see if we already have a lease cached
	leaseID, ok := e.leases[options.Domain][s.Name+node.ID]
	e.Unlock()

	if !ok {
		// missing lease, check if the key exists
		ctx, cancel := context.WithTimeout(context.Background(), e.options.Timeout)
		defer cancel()

		// look for the existing key
		key := nodePath(options.Domain, s.Name, node.ID)
		rsp, err := e.client.Get(ctx, key, clientv3.WithSerializable())
		if err != nil {
			return err
		}

		// get the existing lease
		for _, kv := range rsp.Kvs {
			if kv.Lease > 0 {
				leaseID = clientv3.LeaseID(kv.Lease)

				// decode the existing node
				srv := decode(kv.Value)
				if srv == nil || len(srv.Nodes) == 0 {
					continue
				}

				// create hash of service; uint64
				h, err := hash.Hash(srv.Nodes[0], nil)
				if err != nil {
					continue
				}

				// save the info
				e.Lock()
				e.leases[options.Domain][s.Name+node.ID] = leaseID
				e.reg[options.Domain][s.Name+node.ID] = h
				e.Unlock()

				break
			}
		}
	}

	var leaseNotFound bool

	// renew the lease if it exists
	if leaseID > 0 {
		if logger.V(logger.TraceLevel) {
			logger.Tracef(e.options.Context, "Renewing existing lease for %s %d", s.Name, leaseID)
		}

		if _, err := e.client.KeepAliveOnce(context.TODO(), leaseID); err != nil {
			if err != rpctypes.ErrLeaseNotFound {
				return err
			}

			if logger.V(logger.TraceLevel) {
				logger.Tracef(e.options.Context, "Lease not found for %s %d", s.Name, leaseID)
			}

			// lease not found do register
			leaseNotFound = true
		}
	}

	// create hash of service; uint64
	h, err := hash.Hash(node, nil)
	if err != nil {
		return err
	}

	// get existing hash for the service node
	e.RLock()
	v, ok := e.reg[options.Domain][s.Name+node.ID]
	e.RUnlock()

	// the service is unchanged, skip registering
	if ok && v == h && !leaseNotFound {
		if logger.V(logger.TraceLevel) {
			logger.Tracef(e.options.Context, "Service %s node %s unchanged skipping registration", s.Name, node.ID)
		}
		return nil
	}

	// add domain to the service metadata so it can be determined when doing wildcard queries
	if s.Metadata == nil {
		s.Metadata = map[string]string{"domain": options.Domain}
	} else {
		s.Metadata["domain"] = options.Domain
	}

	service := &register.Service{
		Name:      s.Name,
		Version:   s.Version,
		Metadata:  s.Metadata,
		Endpoints: s.Endpoints,
		Nodes:     []*register.Node{node},
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.options.Timeout)
	defer cancel()

	var lgr *clientv3.LeaseGrantResponse
	if options.TTL.Seconds() > 0 {
		// get a lease used to expire keys since we have a ttl
		lgr, err = e.client.Grant(ctx, int64(options.TTL.Seconds()))
		if err != nil {
			return err
		}
	}

	// create an entry for the node
	var putOpts []clientv3.OpOption
	if lgr != nil {
		putOpts = append(putOpts, clientv3.WithLease(lgr.ID))

		if logger.V(logger.TraceLevel) {
			logger.Tracef(e.options.Context, "Registering %s id %s with lease %v and leaseID %v and ttl %v", service.Name, node.ID, lgr, lgr.ID, options.TTL)
		}
	} else if logger.V(logger.TraceLevel) {
		logger.Tracef(e.options.Context, "Registering %s id %s without lease", service.Name, node.ID)
	}

	key := nodePath(options.Domain, s.Name, node.ID)
	if _, err = e.client.Put(ctx, key, encode(service), putOpts...); err != nil {
		return err
	}

	e.Lock()
	// save our hash of the service
	e.reg[options.Domain][s.Name+node.ID] = h
	// save our leaseID of the service
	if lgr != nil {
		e.leases[options.Domain][s.Name+node.ID] = lgr.ID
	}
	e.Unlock()

	return nil
}

func (e *etcdRegister) Deregister(ctx context.Context, s *register.Service, opts ...register.DeregisterOption) error {
	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	// parse the options
	options := register.NewDeregisterOptions(opts...)

	for _, node := range s.Nodes {
		e.Lock()
		// delete our hash of the service
		nodes, ok := e.reg[options.Domain]
		if ok {
			delete(nodes, s.Name+node.ID)
			e.reg[options.Domain] = nodes
		}

		// delete our lease of the service
		leases, ok := e.leases[options.Domain]
		if ok {
			delete(leases, s.Name+node.ID)
			e.leases[options.Domain] = leases
		}
		e.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), e.options.Timeout)
		defer cancel()

		if logger.V(logger.TraceLevel) {
			logger.Tracef(e.options.Context, "Deregistering %s id %s", s.Name, node.ID)
		}

		if _, err := e.client.Delete(ctx, nodePath(options.Domain, s.Name, node.ID)); err != nil {
			return err
		}
	}

	return nil
}

func (e *etcdRegister) Register(ctx context.Context, s *register.Service, opts ...register.RegisterOption) error {
	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	var gerr error

	// register each node individually
	for _, node := range s.Nodes {
		if err := e.registerNode(s, node, opts...); err != nil {
			gerr = err
		}
	}

	return gerr
}

func (e *etcdRegister) LookupService(ctx context.Context, name string, opts ...register.LookupOption) ([]*register.Service, error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, e.options.Timeout)
	defer cancel()

	// parse the options and fallback to the default domain
	options := register.NewLookupOptions(opts...)

	var results []*mvccpb.KeyValue

	// TODO: refactorout wildcard, this is an incredibly expensive operation
	if options.Domain == register.WildcardDomain {
		rsp, err := e.client.Get(ctx, DefaultPrefix, clientv3.WithPrefix(), clientv3.WithSerializable())
		if err != nil {
			return nil, err
		}

		// filter the results for the key we care about
		for _, kv := range rsp.Kvs {
			// if the key does not contain the name then pass
			_, service, ok := getName(string(kv.Key), DefaultPrefix)
			if !ok || service != name {
				continue
			}

			// save the result if its what we expect
			results = append(results, kv)
		}
	} else {
		prefix := servicePath(options.Domain, name) + "/"
		rsp, err := e.client.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithSerializable())
		if err != nil {
			return nil, err
		}
		results = rsp.Kvs
	}

	if len(results) == 0 {
		return nil, register.ErrNotFound
	}

	versions := make(map[string]*register.Service)

	for _, n := range results {
		// only process the things we care about
		domain, service, ok := getName(string(n.Key), DefaultPrefix)
		if !ok || service != name {
			continue
		}

		if sn := decode(n.Value); sn != nil {
			// compose a key of name/version/domain
			key := sn.Name + sn.Version + domain

			s, ok := versions[key]
			if !ok {
				s = &register.Service{
					Name:      sn.Name,
					Version:   sn.Version,
					Metadata:  sn.Metadata,
					Endpoints: sn.Endpoints,
				}
				versions[key] = s
			}
			s.Nodes = append(s.Nodes, sn.Nodes...)
		}
	}

	services := make([]*register.Service, 0, len(versions))

	for _, service := range versions {
		services = append(services, service)
	}

	return services, nil
}

func (e *etcdRegister) ListServices(ctx context.Context, opts ...register.ListOption) ([]*register.Service, error) {
	// parse the options
	options := register.NewListOptions(opts...)

	// determine the prefix
	var p string
	if options.Domain == register.WildcardDomain {
		p = DefaultPrefix
	} else {
		p = prefixWithDomain(options.Domain)
	}

	nctx, cancel := context.WithTimeout(context.Background(), e.options.Timeout)
	defer cancel()

	rsp, err := e.client.Get(nctx, p, clientv3.WithPrefix(), clientv3.WithSerializable())
	if err != nil {
		return nil, err
	}
	if len(rsp.Kvs) == 0 {
		return []*register.Service{}, nil
	}

	versions := make(map[string]*register.Service)
	for _, n := range rsp.Kvs {
		domain, service, ok := getName(string(n.Key), DefaultPrefix)
		if !ok {
			continue
		}

		sn := decode(n.Value)
		if sn == nil || sn.Name != service {
			continue
		}

		// key based on name/version/domain
		key := sn.Name + sn.Version + domain

		v, ok := versions[key]
		if !ok {
			versions[key] = sn
			continue
		}

		// append to service:version nodes
		v.Nodes = append(v.Nodes, sn.Nodes...)
	}

	services := make([]*register.Service, 0, len(versions))
	for _, service := range versions {
		services = append(services, service)
	}

	// sort the services
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	return services, nil
}

func (e *etcdRegister) Watch(ctx context.Context, opts ...register.WatchOption) (register.Watcher, error) {
	cli, err := newClient(e)
	if err != nil {
		return nil, err
	}
	return newEtcdWatcher(cli, e.options.Timeout, opts...)
}

func (e *etcdRegister) String() string {
	return "etcd"
}

func (e *etcdRegister) Name() string {
	return e.options.Name
}
