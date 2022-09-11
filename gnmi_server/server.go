package gnmi

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Azure/sonic-mgmt-common/translib"
	"github.com/sonic-net/sonic-gnmi/common_utils"
	spb "github.com/sonic-net/sonic-gnmi/proto"
	spb_gnoi "github.com/sonic-net/sonic-gnmi/proto/gnoi"
	spb_jwt_gnoi "github.com/sonic-net/sonic-gnmi/proto/gnoi/jwt"
	sdc "github.com/sonic-net/sonic-gnmi/sonic_data_client"
	log "github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	gnmi_extpb "github.com/openconfig/gnmi/proto/gnmi_ext"
	gnoi_system_pb "github.com/openconfig/gnoi/system"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"net"
	"strings"
	"sync"
)

var (
	supportedEncodings = []gnmipb.Encoding{gnmipb.Encoding_JSON, gnmipb.Encoding_JSON_IETF}
)

// Server manages a single gNMI Server implementation. Each client that connects
// via Subscribe or Get will receive a stream of updates based on the requested
// path. Set request is processed by server too.
type Server struct {
	s       *grpc.Server
	lis     net.Listener
	config  *Config
	cMu     sync.Mutex
	clients map[string]*Client
}
type AuthTypes map[string]bool

// Config is a collection of values for Server
type Config struct {
	// Port for the Server to listen on. If 0 or unset the Server will pick a port
	// for this Server.
	Port     int64
	LogLevel int
	UserAuth AuthTypes
	TranslibEnable  bool
}

var AuthLock sync.Mutex

func (i AuthTypes) String() string {
	if i["none"] {
		return ""
	}
	b := new(bytes.Buffer)
	for key, value := range i {
		if value {
			fmt.Fprintf(b, "%s ", key)
		}
	}
	return b.String()
}

func (i AuthTypes) Any() bool {
	if i["none"] {
		return false
	}
	for _, value := range i {
		if value {
			return true
		}
	}
	return false
}

func (i AuthTypes) Enabled(mode string) bool {
	if i["none"] {
		return false
	}
	if value, exist := i[mode]; exist && value {
		return true
	}
	return false
}

func (i AuthTypes) Set(mode string) error {
	modes := strings.Split(mode, ",")
	for _, m := range modes {
		m = strings.Trim(m, " ")
		if m == "none" || m == "" {
			i["none"] = true
			return nil
		}

		if _, exist := i[m]; !exist {
			return fmt.Errorf("Expecting one or more of 'cert', 'password' or 'jwt'")
		}
		i[m] = true
	}
	return nil
}

func (i AuthTypes) Unset(mode string) error {
	modes := strings.Split(mode, ",")
	for _, m := range modes {
		m = strings.Trim(m, " ")
		if _, exist := i[m]; !exist {
			return fmt.Errorf("Expecting one or more of 'cert', 'password' or 'jwt'")
		}
		i[m] = false
	}
	return nil
}

// New returns an initialized Server.
func NewServer(config *Config, opts []grpc.ServerOption) (*Server, error) {
	if config == nil {
		return nil, errors.New("config not provided")
	}

	s := grpc.NewServer(opts...)
	reflection.Register(s)

	srv := &Server{
		s:       s,
		config:  config,
		clients: map[string]*Client{},
	}
	var err error
	if srv.config.Port < 0 {
		srv.config.Port = 0
	}
	srv.lis, err = net.Listen("tcp", fmt.Sprintf(":%d", srv.config.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to open listener port %d: %v", srv.config.Port, err)
	}
	gnmipb.RegisterGNMIServer(srv.s, srv)
	spb_jwt_gnoi.RegisterSonicJwtServiceServer(srv.s, srv)
	if srv.config.TranslibEnable {
		gnoi_system_pb.RegisterSystemServer(srv.s, srv)
		spb_gnoi.RegisterSonicServiceServer(srv.s, srv)
	}
	log.V(1).Infof("Created Server on %s, read-only: %t", srv.Address(), !srv.config.TranslibEnable)
	return srv, nil
}

// Serve will start the Server serving and block until closed.
func (srv *Server) Serve() error {
	s := srv.s
	if s == nil {
		return fmt.Errorf("Serve() failed: not initialized")
	}
	return srv.s.Serve(srv.lis)
}

// Address returns the port the Server is listening to.
func (srv *Server) Address() string {
	addr := srv.lis.Addr().String()
	return strings.Replace(addr, "[::]", "localhost", 1)
}

// Port returns the port the Server is listening to.
func (srv *Server) Port() int64 {
	return srv.config.Port
}

func authenticate(UserAuth AuthTypes, ctx context.Context) (context.Context, error) {
	var err error
	success := false
	rc, ctx := common_utils.GetContext(ctx)
	if !UserAuth.Any() {
		//No Auth enabled
		rc.Auth.AuthEnabled = false
		return ctx, nil
	}
	rc.Auth.AuthEnabled = true
	if UserAuth.Enabled("password") {
		ctx, err = BasicAuthenAndAuthor(ctx)
		if err == nil {
			success = true
		}
	}
	if !success && UserAuth.Enabled("jwt") {
		_, ctx, err = JwtAuthenAndAuthor(ctx)
		if err == nil {
			success = true
		}
	}
	if !success && UserAuth.Enabled("cert") {
		ctx, err = ClientCertAuthenAndAuthor(ctx)
		if err == nil {
			success = true
		}
	}

	//Allow for future authentication mechanisms here...

	if !success {
		return ctx, status.Error(codes.Unauthenticated, "Unauthenticated")
	}
	log.V(5).Infof("authenticate user %v, roles %v", rc.Auth.User, rc.Auth.Roles)

	return ctx, nil
}

// Subscribe implements the gNMI Subscribe RPC.
func (s *Server) Subscribe(stream gnmipb.GNMI_SubscribeServer) error {
	ctx := stream.Context()
	ctx, err := authenticate(s.config.UserAuth, ctx)
	if err != nil {
		return err
	}

	pr, ok := peer.FromContext(ctx)
	if !ok {
		return grpc.Errorf(codes.InvalidArgument, "failed to get peer from ctx")
		//return fmt.Errorf("failed to get peer from ctx")
	}
	if pr.Addr == net.Addr(nil) {
		return grpc.Errorf(codes.InvalidArgument, "failed to get peer address")
	}

	/* TODO: authorize the user
	msg, ok := credentials.AuthorizeUser(ctx)
	if !ok {
		log.Infof("denied a Set request: %v", msg)
		return nil, status.Error(codes.PermissionDenied, msg)
	}
	*/

	c := NewClient(pr.Addr)

	c.setLogLevel(s.config.LogLevel)

	s.cMu.Lock()
	if oc, ok := s.clients[c.String()]; ok {
		log.V(2).Infof("Delete duplicate client %s", oc)
		oc.Close()
		delete(s.clients, c.String())
	}
	s.clients[c.String()] = c
	s.cMu.Unlock()

	err = c.Run(stream)
	s.cMu.Lock()
	delete(s.clients, c.String())
	s.cMu.Unlock()

	log.Flush()
	return err
}

// checkEncodingAndModel checks whether encoding and models are supported by the server. Return error if anything is unsupported.
func (s *Server) checkEncodingAndModel(encoding gnmipb.Encoding, models []*gnmipb.ModelData) error {
	hasSupportedEncoding := false
	for _, supportedEncoding := range supportedEncodings {
		if encoding == supportedEncoding {
			hasSupportedEncoding = true
			break
		}
	}
	if !hasSupportedEncoding {
		return fmt.Errorf("unsupported encoding: %s", gnmipb.Encoding_name[int32(encoding)])
	}

	return nil
}

// Get implements the Get RPC in gNMI spec.
func (s *Server) Get(ctx context.Context, req *gnmipb.GetRequest) (*gnmipb.GetResponse, error) {
	ctx, err := authenticate(s.config.UserAuth, ctx)
	if err != nil {
		return nil, err
	}

	if req.GetType() != gnmipb.GetRequest_ALL {
		return nil, status.Errorf(codes.Unimplemented, "unsupported request type: %s", gnmipb.GetRequest_DataType_name[int32(req.GetType())])
	}

	if err = s.checkEncodingAndModel(req.GetEncoding(), req.GetUseModels()); err != nil {
		return nil, status.Error(codes.Unimplemented, err.Error())
	}

	var target string
	prefix := req.GetPrefix()
	if prefix == nil {
		return nil, status.Error(codes.Unimplemented, "No target specified in prefix")
	} else {
		target = prefix.GetTarget()
		if target == "" {
			return nil, status.Error(codes.Unimplemented, "Empty target data not supported yet")
		}
	}

	paths := req.GetPath()
	extensions := req.GetExtension()
	target = prefix.GetTarget()
	log.V(5).Infof("GetRequest paths: %v", paths)

	var dc sdc.Client

	if target == "OTHERS" {
		dc, err = sdc.NewNonDbClient(paths, prefix)
	} else if _, ok, _, _ := sdc.IsTargetDb(target); ok {
		dc, err = sdc.NewDbClient(paths, prefix)
	} else {
		/* If no prefix target is specified create new Transl Data Client . */
		dc, err = sdc.NewTranslClient(prefix, paths, ctx, extensions)
	}

	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	notifications := make([]*gnmipb.Notification, len(paths))
	spbValues, err := dc.Get(nil)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	for index, spbValue := range spbValues {
		update := &gnmipb.Update{
			Path: spbValue.GetPath(),
			Val:  spbValue.GetVal(),
		}

		notifications[index] = &gnmipb.Notification{
			Timestamp: spbValue.GetTimestamp(),
			Prefix:    prefix,
			Update:    []*gnmipb.Update{update},
		}
	}
	return &gnmipb.GetResponse{Notification: notifications}, nil
}

func (s *Server) Set(ctx context.Context, req *gnmipb.SetRequest) (*gnmipb.SetResponse, error) {
	ctx, err := authenticate(s.config.UserAuth, ctx)
	if err != nil {
		return nil, err
	}
	var results []*gnmipb.UpdateResult

	/* Fetch the prefix. */
	prefix := req.GetPrefix()
	extensions := req.GetExtension()
	/* Create Transl client. */
	dc, _ := sdc.NewTranslClient(prefix, nil, ctx, extensions)

	/* DELETE */
	for _, path := range req.GetDelete() {
		log.V(2).Infof("Delete path: %v", path)

		res := gnmipb.UpdateResult{
			Path: path,
			Op:   gnmipb.UpdateResult_DELETE,
		}

		/* Add to Set response results. */
		results = append(results, &res)

	}

	/* REPLACE */
	for _, path := range req.GetReplace() {
		log.V(2).Infof("Replace path: %v ", path)

		res := gnmipb.UpdateResult{
			Path: path.GetPath(),
			Op:   gnmipb.UpdateResult_REPLACE,
		}
		/* Add to Set response results. */
		results = append(results, &res)
	}

	/* UPDATE */
	for _, path := range req.GetUpdate() {
		log.V(2).Infof("Update path: %v ", path)

		res := gnmipb.UpdateResult{
			Path: path.GetPath(),
			Op:   gnmipb.UpdateResult_UPDATE,
		}
		/* Add to Set response results. */
		results = append(results, &res)
	}
	if s.config.TranslibEnable {
		err = dc.Set(req.GetDelete(), req.GetReplace(), req.GetUpdate())
	} else {
		return nil, grpc.Errorf(codes.Unimplemented, "Telemetry is in read-only mode")
	}


	return &gnmipb.SetResponse{
		Prefix:   req.GetPrefix(),
		Response: results,
	}, err

}

func (s *Server) Capabilities(ctx context.Context, req *gnmipb.CapabilityRequest) (*gnmipb.CapabilityResponse, error) {
	ctx, err := authenticate(s.config.UserAuth, ctx)
	if err != nil {
		return nil, err
	}
	extensions := req.GetExtension()
	dc, _ := sdc.NewTranslClient(nil, nil, ctx, extensions)

	/* Fetch the client capabitlities. */
	supportedModels := dc.Capabilities()
	suppModels := make([]*gnmipb.ModelData, len(supportedModels))

	for index, model := range supportedModels {
		suppModels[index] = &gnmipb.ModelData{
			Name:         model.Name,
			Organization: model.Organization,
			Version:      model.Version,
		}
	}

	sup_bver := spb.SupportedBundleVersions{
		BundleVersion: translib.GetYangBundleVersion().String(),
		BaseVersion:   translib.GetYangBaseVersion().String(),
	}
	sup_msg, _ := proto.Marshal(&sup_bver)
	ext := gnmi_extpb.Extension{}
	ext.Ext = &gnmi_extpb.Extension_RegisteredExt{
		RegisteredExt: &gnmi_extpb.RegisteredExtension{
			Id:  spb.SUPPORTED_VERSIONS_EXT,
			Msg: sup_msg}}
	exts := []*gnmi_extpb.Extension{&ext}

	return &gnmipb.CapabilityResponse{SupportedModels: suppModels,
		SupportedEncodings: supportedEncodings,
		GNMIVersion:        "0.7.0",
		Extension:          exts}, nil
}
