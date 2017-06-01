package managehttpserver

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/context"

	"github.com/golang/glog"

	"github.com/openconnectio/openmanage/common"
	"github.com/openconnectio/openmanage/containersvc"
	"github.com/openconnectio/openmanage/db"
	"github.com/openconnectio/openmanage/dns"
	"github.com/openconnectio/openmanage/manage"
	"github.com/openconnectio/openmanage/manage/service"
	"github.com/openconnectio/openmanage/server"
	"github.com/openconnectio/openmanage/utils"
)

// The ManageHTTPServer is the management http server for the service management.
// It will run in a container, publish a well-known DNS name, which could be accessed
// publicly or privately depend on the customer.
//
// The service creation needs to talk to DB (the controldb, dynamodb, etc), which is
// accessable inside the cloud platform (aws, etc). For example, the controldb running
// as the container in AWS ECS, and accessable via the private DNS name.
// The ManageHTTPServer will accept the calls from the admin, and talk with DB. This also
// enhance the security. The ManageHTTPServer REST APIs are the only exposed access to
// the cluster.
//
// AWS VPC is the region wide concept. One VPC could cross all AZs of the region.
// The Route53 HostedZone is global concept, one VPC could associate with multiple VPCs.
//
// For the stateful application across multiple Regions, we will have the federation mode.
// Each Region has its own container cluster. Each cluster has its own DB service and
// hosted zone. One federation HostedZone is created for all clusters.
// Note: the federation HostedZone could include multiple VPCs at multiple Regions.
type ManageHTTPServer struct {
	region  string
	cluster string

	dbIns           db.DB
	serverInfo      server.Info
	containersvcIns containersvc.ContainerSvc
	svc             *manageservice.ManageService
}

// NewManageHTTPServer creates a ManageHTTPServer instance
func NewManageHTTPServer(cluster string, dbIns db.DB, dnsIns dns.DNS, serverIns server.Server,
	serverInfo server.Info, containersvcIns containersvc.ContainerSvc) *ManageHTTPServer {
	svc := manageservice.NewManageService(dbIns, serverIns, dnsIns)
	s := &ManageHTTPServer{
		region:          serverInfo.GetLocalRegion(),
		cluster:         cluster,
		dbIns:           dbIns,
		serverInfo:      serverInfo,
		containersvcIns: containersvcIns,
		svc:             svc,
	}
	return s
}

func (s *ManageHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// generate uuid as request id
	requuid := utils.GenRequestUUID()

	w.Header().Set(manage.RequestID, requuid)
	w.Header().Set(manage.Server, common.SystemName)
	w.Header().Set(manage.ContentType, manage.JsonContentType)

	unescapedURL, err := url.QueryUnescape(r.RequestURI)
	if err != nil {
		glog.Errorln("url.QueryUnescape error", err, r.RequestURI, "requuid", requuid, r)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	trimURL := strings.TrimLeft(unescapedURL, "/")

	glog.Infoln("request Method", r.Method, "URL", r.URL, "Host", r.Host, "requuid", requuid, "headers", r.Header)

	// make sure body is closed
	defer s.closeBody(r)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	errmsg := ""
	errcode := http.StatusOK
	switch r.Method {
	case http.MethodPost:
		errmsg, errcode = s.putOp(ctx, w, r, trimURL, requuid)
	case http.MethodPut:
		errmsg, errcode = s.putOp(ctx, w, r, trimURL, requuid)
	case http.MethodGet:
		errmsg, errcode = s.getOp(ctx, w, r, trimURL, requuid)
	case http.MethodDelete:
		errmsg, errcode = s.delOp(ctx, w, r, trimURL, requuid)
	default:
		errmsg = http.StatusText(http.StatusNotImplemented)
		errcode = http.StatusNotImplemented
	}

	if errcode != http.StatusOK {
		http.Error(w, errmsg, errcode)
	}
}

// PUT/POST to do the service related operations. The necessary parameters should be
// passed in the http headers.
// Example:
//   PUT /servicename, create a service.
//   PUT /?SetServiceInitialized, mark a service initialized.
func (s *ManageHTTPServer) putOp(ctx context.Context, w http.ResponseWriter, r *http.Request, trimURL string, requuid string) (errmsg string, errcode int) {
	if strings.HasPrefix(trimURL, manage.SpecialOpPrefix) {
		switch trimURL {
		case manage.ServiceInitializedOp:
			return s.setServiceInitialized(ctx, w, r, requuid)
		case manage.RunTaskOp:
			return s.runTask(ctx, w, r, requuid)
		default:
			return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
		}
	} else {
		return s.createService(ctx, w, r, trimURL, requuid)
	}
}

func (s *ManageHTTPServer) setServiceInitialized(ctx context.Context, w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		glog.Errorln("setServiceInitialized read body error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	req := &manage.ServiceCommonRequest{}
	err = json.Unmarshal(b, req)
	if err != nil {
		glog.Errorln("setServiceInitialized decode request error", err, "requuid", requuid, string(b[:]))
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("setServiceInitialized invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = s.svc.SetServiceInitialized(ctx, s.cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("setServiceInitialized error", err, "service", req.ServiceName, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("set service", req.ServiceName, "initialized, requuid", requuid)

	w.WriteHeader(http.StatusOK)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) createService(ctx context.Context, w http.ResponseWriter,
	r *http.Request, servicename string, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CreateServiceRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("createService decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region ||
		req.Service.ServiceName != servicename {
		glog.Errorln("createService invalid request, local cluster", s.cluster, "region",
			s.region, "service", servicename, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	// create the service in the control plane
	domain := dns.GenDefaultDomainName(s.cluster)
	vpcID := s.serverInfo.GetLocalVpcID()

	serviceUUID, err := s.svc.CreateService(ctx, req, domain, vpcID)
	if err != nil {
		glog.Errorln("create service error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	// create the service in the container platform
	exist, err := s.containersvcIns.IsServiceExist(ctx, req.Service.Cluster, req.Service.ServiceName)
	if err != nil {
		glog.Errorln("check container service exist error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}
	if !exist {
		opts := s.genCreateServiceOptions(req, serviceUUID)
		err = s.containersvcIns.CreateService(ctx, opts)
		if err != nil {
			glog.Errorln("CreateService error", err, "requuid", requuid, req.Service)
			return manage.ConvertToHTTPError(err)
		}
	}

	glog.Infoln("create service done, serviceUUID", serviceUUID, "requuid", requuid, req.Service)
	return "", http.StatusOK
}

func (s *ManageHTTPServer) genCreateServiceOptions(req *manage.CreateServiceRequest, serviceUUID string) *containersvc.CreateServiceOptions {
	commonOpts := &containersvc.CommonOptions{
		Cluster:        req.Service.Cluster,
		ServiceName:    req.Service.ServiceName,
		ServiceUUID:    serviceUUID,
		ContainerImage: req.ContainerImage,
		Resource:       req.Resource,
	}

	createOpts := &containersvc.CreateServiceOptions{
		Common:        commonOpts,
		ContainerPath: req.ContainerPath,
		Port:          req.Port,
		Replicas:      req.Replicas,
	}

	return createOpts
}

// Get one service, GET /servicename. Or list services, Get / or /?list-type=1, and additional parameters in headers
func (s *ManageHTTPServer) getOp(ctx context.Context, w http.ResponseWriter,
	r *http.Request, trimURL string, requuid string) (errmsg string, errcode int) {
	if strings.HasPrefix(trimURL, manage.SpecialOpPrefix) {
		switch trimURL {
		case manage.ListServiceOp:
			return s.listServices(ctx, w, r, requuid)

		case manage.ListVolumeOp:
			return s.listVolumes(ctx, w, r, requuid)

		case manage.GetServiceStatusOp:
			return s.getServiceStatus(ctx, w, r, requuid)

		case manage.GetTaskStatusOp:
			return s.getTaskStatus(ctx, w, r, requuid)

		default:
			return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
		}
	} else {
		// get the detail of one service
		return s.getServiceAttr(ctx, w, r, trimURL, requuid)
	}
}

// Delete one service, DELETE /servicename
func (s *ManageHTTPServer) delOp(ctx context.Context, w http.ResponseWriter, r *http.Request, trimURL string, requuid string) (errmsg string, errcode int) {
	if strings.HasPrefix(trimURL, manage.SpecialOpPrefix) {
		switch trimURL {
		case manage.DeleteTaskOp:
			return s.deleteTask(ctx, w, r, requuid)
		default:
			return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
		}
	} else {
		// get the detail of one service
		return s.deleteService(ctx, w, r, trimURL, requuid)
	}
}

func (s *ManageHTTPServer) deleteService(ctx context.Context, w http.ResponseWriter, r *http.Request, servicename string, requuid string) (errmsg string, errcode int) {
	req := &manage.ServiceCommonRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("deleteService decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region || req.ServiceName != servicename {
		glog.Errorln("deleteService invalid request, local cluster", s.cluster, "region",
			s.region, "service", servicename, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = s.dbIns.DeleteService(ctx, s.cluster, servicename)
	if err != nil {
		glog.Errorln("DeleteService error", err, servicename, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("deleted service", servicename, "requuid", requuid, r)

	w.WriteHeader(http.StatusOK)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) listServices(ctx context.Context, w http.ResponseWriter,
	r *http.Request, requuid string) (errmsg string, errcode int) {
	// no need to support token and MaxKeys, simply returns all as one cluster would
	// not have too many services.
	req := &manage.ListServiceRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("listServices decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("listServices invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	glog.Infoln("listServices, prefix", req.Prefix, "requuid", requuid)

	services, err := s.dbIns.ListServices(ctx, s.cluster)
	if err != nil {
		glog.Errorln("ListServices error", err, "prefix", req.Prefix, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	var serviceAttrs []*common.ServiceAttr
	for _, service := range services {
		if len(req.Prefix) == 0 || strings.HasPrefix(service.ServiceName, req.Prefix) {
			// fetch the detail service attr
			attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
			if err != nil {
				glog.Errorln("GetServiceAttr error", err, service, "requuid", requuid)
				return manage.ConvertToHTTPError(err)
			}

			glog.Infoln("GetServiceAttr", attr, "requuid", requuid)
			serviceAttrs = append(serviceAttrs, attr)
		}
	}

	glog.Infoln("list", len(services), "services, prefix", req.Prefix, "requuid", requuid)

	resp := &manage.ListServiceResponse{Services: serviceAttrs}
	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal ListServiceResponse error", err, "requuid", requuid, req)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) listVolumes(ctx context.Context, w http.ResponseWriter,
	r *http.Request, requuid string) (errmsg string, errcode int) {
	// TODO support token and MaxKeys if necessary.
	req := &manage.ListVolumeRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("listVolumes decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("listVolumes invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	glog.Infoln("listVolumes", req.Service, "requuid", requuid)

	service, err := s.dbIns.GetService(ctx, s.cluster, req.Service.ServiceName)
	if err != nil {
		glog.Errorln("db GetService error", err, req.Service.ServiceName, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	vols, err := s.dbIns.ListVolumes(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("db ListVolumes error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("list", len(vols), "volumes, requuid", requuid, req.Service)

	resp := &manage.ListVolumeResponse{Volumes: vols}
	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal ListVolumeResponse error", err, "requuid", requuid, req)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) getServiceAttr(ctx context.Context, w http.ResponseWriter,
	r *http.Request, servicename string, requuid string) (errmsg string, errcode int) {
	// no need to support token and MaxKeys, simply returns all volumes. Assume one volume
	// attribute is 1KB. If the service has 1000 volumes, the whole list would be 1MB.
	req := &manage.ServiceCommonRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("getServiceAttr decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region || req.ServiceName != servicename {
		glog.Errorln("getServiceAttr invalid request, local cluster", s.cluster, "region",
			s.region, "service", servicename, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	service, err := s.dbIns.GetService(ctx, s.cluster, servicename)
	if err != nil {
		glog.Errorln("GetService error", err, servicename, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, service, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	resp := &manage.GetServiceAttributesResponse{Service: attr}

	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal GetServiceAttributesResponse error", err, attr, "requuid", requuid)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) getServiceStatus(ctx context.Context,
	w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	req := &manage.ServiceCommonRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("getServiceStatus decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("invalid request, local cluster", s.cluster, "region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	status, err := s.containersvcIns.GetServiceStatus(ctx, req.Cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("GetServiceStatus error", err, "requuid", requuid, req)
		return manage.ConvertToHTTPError(err)
	}

	resp := &common.ServiceStatus{
		RunningCount: status.RunningCount,
		DesiredCount: status.DesiredCount,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal error", err, "requuid", requuid, req)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	glog.Infoln("get service status", status, "requuid", requuid, req)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) closeBody(r *http.Request) {
	if r.Body != nil {
		r.Body.Close()
	}
}

func (s *ManageHTTPServer) runTask(ctx context.Context, w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.RunTaskRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("runTask decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("invalid request, local cluster", s.cluster, "region",
			s.region, "requuid", requuid, "task type", req.TaskType, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	svc, err := s.dbIns.GetService(ctx, req.Service.Cluster, req.Service.ServiceName)
	if err != nil {
		glog.Errorln("GetService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	commonOpts := &containersvc.CommonOptions{
		Cluster:        req.Service.Cluster,
		ServiceName:    req.Service.ServiceName,
		ServiceUUID:    svc.ServiceUUID,
		ContainerImage: req.ContainerImage,
		Resource:       req.Resource,
	}

	opts := &containersvc.RunTaskOptions{
		Common:   commonOpts,
		TaskType: req.TaskType,
		Envkvs:   req.Envkvs,
	}

	taskID, err := s.containersvcIns.RunTask(ctx, opts)
	if err != nil {
		glog.Errorln("RunTask error", err, "requuid", requuid, req.Service, svc)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("run task", taskID, "requuid", requuid, req.Service, svc)

	resp := &manage.RunTaskResponse{
		TaskID: taskID,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal ServiceRunningStatus error", err, "requuid", requuid, req)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) getTaskStatus(ctx context.Context, w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.GetTaskStatusRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region || len(req.TaskID) == 0 {
		glog.Errorln("invalid request, local cluster", s.cluster, "region",
			s.region, "requuid", requuid, "taskID", req.TaskID, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	taskStatus, err := s.containersvcIns.GetTaskStatus(ctx, s.cluster, req.TaskID)
	if err != nil {
		glog.Errorln("GetTaskStatus error", err, "requuid", requuid, "taskID", req.TaskID, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("get task", req.TaskID, "status", taskStatus, "requuid", requuid, req.Service)

	resp := &manage.GetTaskStatusResponse{
		Status: taskStatus,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal error", err, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) deleteTask(ctx context.Context, w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.DeleteTaskRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region ||
		len(req.Service.ServiceName) == 0 || len(req.TaskType) == 0 {
		glog.Errorln("invalid request, local cluster", s.cluster, "region",
			s.region, "requuid", requuid, "taskID", req.TaskType, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = s.containersvcIns.DeleteTask(ctx, s.cluster, req.Service.ServiceName, req.TaskType)
	if err != nil {
		glog.Errorln("DeleteTask error", err, "requuid", requuid, "TaskType", req.TaskType, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("deleted task, requuid", requuid, "TaskType", req.TaskType, req.Service)
	return "", http.StatusOK
}