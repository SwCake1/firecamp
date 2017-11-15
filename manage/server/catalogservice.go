package manageserver

import (
	"encoding/json"
	"net/http"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/cloudstax/firecamp/catalog"
	"github.com/cloudstax/firecamp/catalog/cassandra"
	"github.com/cloudstax/firecamp/catalog/consul"
	"github.com/cloudstax/firecamp/catalog/couchdb"
	"github.com/cloudstax/firecamp/catalog/elasticsearch"
	"github.com/cloudstax/firecamp/catalog/kafka"
	"github.com/cloudstax/firecamp/catalog/kibana"
	"github.com/cloudstax/firecamp/catalog/logstash"
	"github.com/cloudstax/firecamp/catalog/mongodb"
	"github.com/cloudstax/firecamp/catalog/postgres"
	"github.com/cloudstax/firecamp/catalog/redis"
	"github.com/cloudstax/firecamp/catalog/zookeeper"
	"github.com/cloudstax/firecamp/common"
	"github.com/cloudstax/firecamp/db"
	"github.com/cloudstax/firecamp/dns"
	"github.com/cloudstax/firecamp/manage"
	"github.com/cloudstax/firecamp/utils"
)

func (s *ManageHTTPServer) putCatalogServiceOp(ctx context.Context, w http.ResponseWriter,
	r *http.Request, trimURL string, requuid string) (errmsg string, errcode int) {
	switch trimURL {
	case manage.CatalogCreateMongoDBOp:
		return s.createMongoDBService(ctx, r, requuid)
	case manage.CatalogCreatePostgreSQLOp:
		return s.createPGService(ctx, r, requuid)
	case manage.CatalogCreateCassandraOp:
		return s.createCasService(ctx, r, requuid)
	case manage.CatalogCreateZooKeeperOp:
		return s.createZkService(ctx, r, requuid)
	case manage.CatalogCreateKafkaOp:
		return s.createKafkaService(ctx, r, requuid)
	case manage.CatalogCreateRedisOp:
		return s.createRedisService(ctx, r, requuid)
	case manage.CatalogCreateCouchDBOp:
		return s.createCouchDBService(ctx, r, requuid)
	case manage.CatalogCreateConsulOp:
		return s.createConsulService(ctx, w, r, requuid)
	case manage.CatalogCreateElasticSearchOp:
		return s.createElasticSearchService(ctx, r, requuid)
	case manage.CatalogCreateKibanaOp:
		return s.createKibanaService(ctx, r, requuid)
	case manage.CatalogCreateLogstashOp:
		return s.createLogstashService(ctx, r, requuid)
	case manage.CatalogSetServiceInitOp:
		return s.catalogSetServiceInit(ctx, r, requuid)
	case manage.CatalogSetRedisInitOp:
		return s.setRedisInit(ctx, r, requuid)
	default:
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}
}

func (s *ManageHTTPServer) getCatalogServiceOp(ctx context.Context,
	w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCheckServiceInitRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCheckServiceInitRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCheckServiceInitRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	// get service uuid
	service, err := s.dbIns.GetService(ctx, s.cluster, req.Service.ServiceName)
	if err != nil {
		glog.Errorln("GetService", req.Service.ServiceName, req.ServiceType, "error", err, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	// check if the init task is running
	initialized := false
	hasTask, statusMsg := s.catalogSvcInit.hasInitTask(ctx, service.ServiceUUID)
	if hasTask {
		glog.Infoln("The service", req.Service.ServiceName, req.ServiceType,
			"is under initialization, requuid", requuid)
	} else {
		// no init task is running, check if the service is initialized
		glog.Infoln("No init task for service", req.Service.ServiceName, req.ServiceType, "requuid", requuid)

		attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
		if err != nil {
			glog.Errorln("GetServiceAttr error", err, service, "requuid", requuid)
			return manage.ConvertToHTTPError(err)
		}

		glog.Infoln("service attribute", attr, "requuid", requuid)

		switch attr.ServiceStatus {
		case common.ServiceStatusActive:
			initialized = true

		case common.ServiceStatusInitializing:
			// service is not initialized, and no init task is running.
			// This is possible. For example, the manage service node crashes and all in-memory
			// init tasks will be lost. Currently rely on the customer to query service status
			// to trigger the init task again.
			// TODO scan the pending init catalog service at start.

			// trigger the init task.
			switch req.ServiceType {
			case catalog.CatalogService_MongoDB:
				s.addMongoDBInitTask(ctx, req.Service, attr.ServiceUUID, attr.Replicas, req.Admin, req.AdminPasswd, requuid)

			case catalog.CatalogService_PostgreSQL:
				// PG does not require additional init work. set PG initialized
				errmsg, errcode := s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
				if errcode != http.StatusOK {
					return errmsg, errcode
				}
				initialized = true

			case catalog.CatalogService_Cassandra:
				s.addCasInitTask(ctx, req.Service, attr.ServiceUUID, requuid)

			case catalog.CatalogService_ZooKeeper:
				// zookeeper does not require additional init work. set initialized
				errmsg, errcode := s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
				if errcode != http.StatusOK {
					return errmsg, errcode
				}
				initialized = true

			case catalog.CatalogService_Kafka:
				// Kafka does not require additional init work. set initialized
				errmsg, errcode := s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
				if errcode != http.StatusOK {
					return errmsg, errcode
				}
				initialized = true

			case catalog.CatalogService_Redis:
				err = s.addRedisInitTask(ctx, req.Service, attr.ServiceUUID, req.Shards, req.ReplicasPerShard, requuid)
				if err != nil {
					glog.Errorln("addRedisInitTask error", err, "requuid", requuid, req.Service)
					return manage.ConvertToHTTPError(err)
				}

			case catalog.CatalogService_CouchDB:
				s.addCouchDBInitTask(ctx, req.Service, attr.ServiceUUID, attr.Replicas, req.Admin, req.AdminPasswd, requuid)

			default:
				return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
			}

		default:
			glog.Errorln("service is not at active or creating status", attr, "requuid", requuid)
			return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
		}
	}

	resp := &manage.CatalogCheckServiceInitResponse{
		Initialized:   initialized,
		StatusMessage: statusMsg,
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

func (s *ManageHTTPServer) createMongoDBService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateMongoDBRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateMongoDBRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateMongoDBRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = mongodbcatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid request", err, "requuid", requuid, req.Service, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq, err := mongodbcatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Options, req.Resource)
	if err != nil {
		glog.Errorln("mongodbcatalog GenDefaultCreateServiceRequest error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("MongoDBService is created, add the init task, requuid", requuid, req.Service, req.Options.Admin)

	// run the init task in the background
	s.addMongoDBInitTask(ctx, crReq.Service, serviceUUID, req.Options.Replicas, req.Options.Admin, req.Options.AdminPasswd, requuid)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) addMongoDBInitTask(ctx context.Context, req *manage.ServiceCommonRequest,
	serviceUUID string, replicas int64, admin string, adminPasswd string, requuid string) {
	logCfg := s.logIns.CreateLogConfigForStream(ctx, s.cluster, req.ServiceName, serviceUUID, common.TaskTypeInit)
	taskOpts := mongodbcatalog.GenDefaultInitTaskRequest(req, logCfg, serviceUUID, replicas, s.manageurl, admin, adminPasswd)

	task := &serviceTask{
		serviceUUID: serviceUUID,
		serviceName: req.ServiceName,
		serviceType: catalog.CatalogService_MongoDB,
		opts:        taskOpts,
	}

	s.catalogSvcInit.addInitTask(ctx, task)

	glog.Infoln("add init task for service", serviceUUID, "requuid", requuid, req)
}

func (s *ManageHTTPServer) createPGService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreatePostgreSQLRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreatePostgreSQLRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreatePostgreSQLRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = pgcatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid request", err, "requuid", requuid, req.Service, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := pgcatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster, req.Service.ServiceName, req.Resource, req.Options)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created postgresql service", serviceUUID, "requuid", requuid, req.Service)

	// PG does not require additional init work. set PG initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createZkService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateZooKeeperRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateZooKeeperRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateZooKeeperRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := zkcatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Options, req.Resource)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created zookeeper service", serviceUUID, "requuid", requuid, req.Service)

	// zookeeper does not require additional init work. set service initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createKafkaService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateKafkaRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateKafkaRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateKafkaRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	// get the zk service
	zksvc, err := s.dbIns.GetService(ctx, s.cluster, req.Options.ZkServiceName)
	if err != nil {
		glog.Errorln("get zk service", req.Options.ZkServiceName, "error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("get zk service", zksvc, "requuid", requuid)

	zkattr, err := s.dbIns.GetServiceAttr(ctx, zksvc.ServiceUUID)
	if err != nil {
		glog.Errorln("get zk service attr", zksvc.ServiceUUID, "error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	// create the service in the control plane and the container platform
	crReq := kafkacatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Options, req.Resource, zkattr)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created kafka service", serviceUUID, "requuid", requuid, req.Service)

	// kafka does not require additional init work. set service initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createRedisService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateRedisRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateRedisRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateRedisRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	glog.Infoln("create redis service", req.Service, req.Options, req.Resource)

	err = rediscatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("CatalogCreateRedisRequest parameters are not valid, requuid", requuid, req.Service, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := rediscatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	if rediscatalog.IsClusterMode(req.Options.Shards) {
		glog.Infoln("The cluster mode Redis is created, add the init task, requuid", requuid, req.Service, req.Options)

		// for Redis cluster mode, run the init task in the background
		err = s.addRedisInitTask(ctx, crReq.Service, serviceUUID, req.Options.Shards, req.Options.ReplicasPerShard, requuid)
		if err != nil {
			glog.Errorln("addRedisInitTask error", err, "requuid", requuid, req.Service)
			return manage.ConvertToHTTPError(err)
		}

		return "", http.StatusOK
	}

	// redis single instance or master-slave mode does not require additional init work. set service initialized
	glog.Infoln("created Redis service", serviceUUID, "requuid", requuid, req.Service, req.Options)

	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) addRedisInitTask(ctx context.Context, req *manage.ServiceCommonRequest,
	serviceUUID string, shards int64, replicasPerShard int64, requuid string) error {
	logCfg := s.logIns.CreateLogConfigForStream(ctx, s.cluster, req.ServiceName, serviceUUID, common.TaskTypeInit)

	taskOpts, err := rediscatalog.GenDefaultInitTaskRequest(req, logCfg, shards, replicasPerShard, serviceUUID, s.manageurl)
	if err != nil {
		return err
	}

	task := &serviceTask{
		serviceUUID: serviceUUID,
		serviceName: req.ServiceName,
		serviceType: catalog.CatalogService_Redis,
		opts:        taskOpts,
	}

	s.catalogSvcInit.addInitTask(ctx, task)

	glog.Infoln("add init task for Redis service", serviceUUID, "requuid", requuid, req)
	return nil
}

func (s *ManageHTTPServer) createCouchDBService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateCouchDBRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateCouchDBRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateCouchDBRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = couchdbcatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("CatalogCreateCouchDBRequest parameters are not valid, requuid", requuid, req)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := couchdbcatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	// add the init task
	s.addCouchDBInitTask(ctx, crReq.Service, serviceUUID, req.Options.Replicas, req.Options.Admin, req.Options.AdminPasswd, requuid)

	glog.Infoln("created CouchDB service", serviceUUID, "requuid", requuid, req.Service, req.Options)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) addCouchDBInitTask(ctx context.Context, req *manage.ServiceCommonRequest,
	serviceUUID string, replicas int64, admin string, adminPass string, requuid string) {
	logCfg := s.logIns.CreateLogConfigForStream(ctx, s.cluster, req.ServiceName, serviceUUID, common.TaskTypeInit)
	taskOpts := couchdbcatalog.GenDefaultInitTaskRequest(req, logCfg, s.azs, serviceUUID, replicas, s.manageurl, admin, adminPass)

	task := &serviceTask{
		serviceUUID: serviceUUID,
		serviceName: req.ServiceName,
		serviceType: catalog.CatalogService_CouchDB,
		opts:        taskOpts,
	}

	s.catalogSvcInit.addInitTask(ctx, task)

	glog.Infoln("add init task for CouchDB service", serviceUUID, "requuid", requuid, req)
}

func (s *ManageHTTPServer) createConsulService(ctx context.Context, w http.ResponseWriter, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateConsulRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateConsulRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateConsulRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = consulcatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("CatalogCreateConsulRequest parameters are not valid, requuid", requuid, req)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := consulcatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options)

	// create the service in the control plane
	domain := dns.GenDefaultDomainName(s.cluster)
	vpcID := s.serverInfo.GetLocalVpcID()

	serviceUUID, err := s.svc.CreateService(ctx, crReq, domain, vpcID)
	if err != nil {
		glog.Errorln("create service error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("create consul service in the control plane", serviceUUID, "requuid", requuid, req.Service, req.Options)

	serverips, err := s.updateConsulConfigs(ctx, serviceUUID, domain, requuid)
	if err != nil {
		glog.Errorln("updateConsulConfigs error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	err = s.createContainerService(ctx, crReq, serviceUUID, requuid)
	if err != nil {
		glog.Errorln("createContainerService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created Consul service", serviceUUID, "server ips", serverips, "requuid", requuid, req.Service, req.Options)

	// consul does not require additional init work. set service initialized
	errmsg, errcode = s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
	if len(errmsg) != 0 {
		glog.Errorln("setServiceInitialized error", errcode, errmsg, "requuid", requuid, req.Service, req.Options)
		return errmsg, errcode
	}

	resp := &manage.CatalogCreateConsulResponse{ConsulServerIPs: serverips}
	b, err := json.Marshal(resp)
	if err != nil {
		glog.Errorln("Marshal CatalogCreateConsulResponse error", err, "requuid", requuid, req.Service, req.Options)
		return http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError
	}

	w.WriteHeader(http.StatusOK)
	w.Write(b)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) createElasticSearchService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateElasticSearchRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateElasticSearchRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateElasticSearchRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = escatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid elasticsearch create request", err, "requuid", requuid, req)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := escatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created elasticsearch service", serviceUUID, "requuid", requuid, req.Service)

	// elasticsearch does not require additional init work. set service initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createKibanaService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateKibanaRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateKibanaRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateKibanaRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = kibanacatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid kibana create request", err, "requuid", requuid, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// get the dedicated master nodes of the elasticsearch service
	// get the elasticsearch service uuid
	service, err := s.dbIns.GetService(ctx, s.cluster, req.Options.ESServiceName)
	if err != nil {
		glog.Errorln("get the elasticsearch service", req.Options.ESServiceName, "error", err, "requuid", requuid, req.Options)
		return manage.ConvertToHTTPError(err)
	}

	// get the elasticsearch service attr
	attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, "requuid", requuid, service)
		return manage.ConvertToHTTPError(err)
	}

	esNode := escatalog.GetFirstMemberHost(attr.DomainName, attr.ServiceName)

	// create the service in the control plane and the container platform
	crReq := kibanacatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options, esNode)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created kibana service", serviceUUID, "requuid", requuid, req.Service)

	// kibana does not require additional init work. set service initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createLogstashService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateLogstashRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateLogstashRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateLogstashRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = logstashcatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid logstash create request", err, "requuid", requuid, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := logstashcatalog.GenDefaultCreateServiceRequest(s.platform, s.region,
		s.azs, s.cluster, req.Service.ServiceName, req.Resource, req.Options)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created logstash service", serviceUUID, "requuid", requuid, req.Service)

	// logstash does not require additional init work. set service initialized
	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) createCasService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateCassandraRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateCassandraRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Service.Cluster != s.cluster || req.Service.Region != s.region {
		glog.Errorln("CatalogCreateCassandraRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = cascatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("invalid request", err, "requuid", requuid, req.Service, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq := cascatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs,
		s.cluster, req.Service.ServiceName, req.Options, req.Resource)
	serviceUUID, err := s.createCommonService(ctx, crReq, requuid)
	if err != nil {
		glog.Errorln("createCommonService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("Cassandra is created, add the init task, requuid", requuid, req.Service)

	// run the init task in the background
	s.addCasInitTask(ctx, crReq.Service, serviceUUID, requuid)

	return "", http.StatusOK
}

func (s *ManageHTTPServer) addCasInitTask(ctx context.Context,
	req *manage.ServiceCommonRequest, serviceUUID string, requuid string) {
	logCfg := s.logIns.CreateLogConfigForStream(ctx, s.cluster, req.ServiceName, serviceUUID, common.TaskTypeInit)
	taskOpts := cascatalog.GenDefaultInitTaskRequest(req, logCfg, serviceUUID, s.manageurl)

	task := &serviceTask{
		serviceUUID: serviceUUID,
		serviceName: req.ServiceName,
		serviceType: catalog.CatalogService_Cassandra,
		opts:        taskOpts,
	}

	s.catalogSvcInit.addInitTask(ctx, task)

	glog.Infoln("add init task for service", serviceUUID, "requuid", requuid, req)
}

func (s *ManageHTTPServer) catalogSetServiceInit(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogSetServiceInitRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogSetServiceInitRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("CatalogSetServiceInitRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	switch req.ServiceType {
	case catalog.CatalogService_MongoDB:
		return s.setMongoDBInit(ctx, req, requuid)

	case catalog.CatalogService_Cassandra:
		glog.Infoln("set cassandra service initialized, requuid", requuid, req)
		return s.setServiceInitialized(ctx, req.ServiceName, requuid)

	case catalog.CatalogService_CouchDB:
		glog.Infoln("set couchdb service initialized, requuid", requuid, req)
		return s.setServiceInitialized(ctx, req.ServiceName, requuid)

	// other services do not require the init task.
	default:
		glog.Errorln("unknown service type", req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}
}

func (s *ManageHTTPServer) setMongoDBInit(ctx context.Context, req *manage.CatalogSetServiceInitRequest, requuid string) (errmsg string, errcode int) {
	// get service uuid
	service, err := s.dbIns.GetService(ctx, s.cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("GetService", req.ServiceName, req.ServiceType, "error", err, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	// get service attr
	attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, "requuid", requuid, service)
		return manage.ConvertToHTTPError(err)
	}

	// list all serviceMembers
	members, err := s.dbIns.ListServiceMembers(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "serviceUUID", service.ServiceUUID, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("get service", service, "has", len(members), "replicas, requuid", requuid)

	// update the init task status message
	statusMsg := "enable auth for MongoDB"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)

	// update the replica (serviceMember) mongod.conf file
	for _, member := range members {
		for i, cfg := range member.Configs {
			if !mongodbcatalog.IsMongoDBConfFile(cfg.FileName) {
				glog.V(5).Infoln("not mongod.conf file, skip the config, requuid", requuid, cfg)
				continue
			}

			glog.Infoln("enable auth on mongod.conf, requuid", requuid, cfg)

			err = s.enableMongoDBAuth(ctx, cfg, i, member, requuid)
			if err != nil {
				glog.Errorln("enableMongoDBAuth error", err, "requuid", requuid, cfg, member)
				return manage.ConvertToHTTPError(err)
			}

			glog.Infoln("enabled auth for replia, requuid", requuid, member)
			break
		}
	}

	// the config files of all replicas are updated, restart all containers
	glog.Infoln("all replicas are updated, restart all containers, requuid", requuid, req)

	// update the init task status message
	statusMsg = "restarting all MongoDB containers"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)

	err = s.containersvcIns.RestartService(ctx, s.cluster, req.ServiceName, attr.Replicas)
	if err != nil {
		glog.Errorln("RestartService error", err, "requuid", requuid, req)
		return manage.ConvertToHTTPError(err)
	}

	// set service initialized
	glog.Infoln("all containers restarted, set service initialized, requuid", requuid, req)

	return s.setServiceInitialized(ctx, req.ServiceName, requuid)
}

func (s *ManageHTTPServer) enableMongoDBAuth(ctx context.Context,
	cfg *common.MemberConfig, cfgIndex int, member *common.ServiceMember, requuid string) error {
	// fetch the config file
	cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
	if err != nil {
		glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
		return err
	}

	// if auth is enabled, return
	if mongodbcatalog.IsAuthEnabled(cfgfile.Content) {
		glog.Infoln("auth is already enabled in the config file", db.PrintConfigFile(cfgfile), "requuid", requuid, member)
		return nil
	}

	// auth is not enabled, enable it
	newContent := mongodbcatalog.EnableMongoDBAuth(cfgfile.Content)

	return s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
}

func (s *ManageHTTPServer) setRedisInit(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogSetRedisInitRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogSetRedisInitRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("CatalogSetRedisInitRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	glog.Infoln("setRedisInit", req.ServiceName, "first node id mapping", req.NodeIds[0], "total", len(req.NodeIds), "requuid", requuid)

	// get service uuid
	service, err := s.dbIns.GetService(ctx, s.cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("GetService", req, "error", err, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	// get service attr
	attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, "requuid", requuid, service)
		return manage.ConvertToHTTPError(err)
	}

	// list all serviceMembers
	members, err := s.dbIns.ListServiceMembers(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "serviceUUID", service.ServiceUUID, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("get service", service, "has", len(members), "replicas, requuid", requuid)

	// update the init task status message
	statusMsg := "create the member to Redis nodeID mapping for the Redis cluster"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)

	// gengerate cluster info config file
	clusterInfoCfg := rediscatalog.CreateClusterInfoFile(req.NodeIds)

	for _, member := range members {
		// create the cluster.info file for every member
		newMember, err := s.createRedisClusterFile(ctx, member, clusterInfoCfg, requuid)
		if err != nil {
			glog.Errorln("createRedisClusterFile error", err, "requuid", requuid, member)
			return manage.ConvertToHTTPError(err)
		}

		// update the redis.conf file
		for i, cfg := range newMember.Configs {
			if !rediscatalog.IsRedisConfFile(cfg.FileName) {
				glog.V(5).Infoln("not redis.conf file, skip the config, requuid", requuid, cfg)
				continue
			}

			glog.Infoln("enable auth on redis.conf, requuid", requuid, cfg, newMember)

			err = s.updateRedisConfigs(ctx, cfg, i, newMember, requuid)
			if err != nil {
				glog.Errorln("updateRedisConfigs error", err, "requuid", requuid, cfg, newMember)
				return manage.ConvertToHTTPError(err)
			}

			glog.Infoln("updated redis.conf for member, requuid", requuid, newMember)
			break
		}
	}

	// the config files of all replicas are updated, restart all containers
	glog.Infoln("all replicas are updated, restart all containers, requuid", requuid, req)

	// update the init task status message
	statusMsg = "restarting all containers"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)

	err = s.containersvcIns.RestartService(ctx, s.cluster, req.ServiceName, attr.Replicas)
	if err != nil {
		glog.Errorln("RestartService error", err, "requuid", requuid, req)
		return manage.ConvertToHTTPError(err)
	}

	// set service initialized
	glog.Infoln("all containers restarted, set service initialized, requuid", requuid, req)

	return s.setServiceInitialized(ctx, req.ServiceName, requuid)
}

func (s *ManageHTTPServer) createRedisClusterFile(ctx context.Context, member *common.ServiceMember,
	cfg *manage.ReplicaConfigFile, requuid string) (newMember *common.ServiceMember, err error) {

	// check if member has the cluster info file, as failure could happen at any time and init task will be retried.
	for _, c := range member.Configs {
		if rediscatalog.IsClusterInfoFile(c.FileName) {
			chksum := utils.GenMD5(cfg.Content)
			if c.FileMD5 != chksum {
				// this is an unknown internal error. the cluster info content should be the same between retries.
				glog.Errorln("Redis cluster file exist but content not match, new content", cfg.Content, chksum,
					"existing config", c, "requuid", requuid, member)
				return nil, common.ErrConfigMismatch
			}

			glog.Infoln("Redis cluster file is already created for member", member.MemberName,
				"service", member.ServiceUUID, "requuid", requuid)
			return member, nil
		}
	}

	// the cluster info file not exist, create it
	version := int64(0)
	fileID := utils.GenMemberConfigFileID(member.MemberName, cfg.FileName, version)
	initcfgfile := db.CreateInitialConfigFile(member.ServiceUUID, fileID, cfg.FileName, cfg.FileMode, cfg.Content)
	cfgfile, err := manage.CreateConfigFile(ctx, s.dbIns, initcfgfile, requuid)
	if err != nil {
		glog.Errorln("createConfigFile error", err, "fileID", fileID,
			"service", member.ServiceUUID, "member", member.MemberName, "requuid", requuid)
		return nil, err
	}

	glog.Infoln("created the Redis cluster config file, requuid", requuid, db.PrintConfigFile(cfgfile))

	// add the new config file to ServiceMember
	config := &common.MemberConfig{FileName: cfg.FileName, FileID: fileID, FileMD5: cfgfile.FileMD5}

	newConfigs := db.CopyMemberConfigs(member.Configs)
	newConfigs = append(newConfigs, config)

	newMember = db.UpdateServiceMemberConfigs(member, newConfigs)
	err = s.dbIns.UpdateServiceMember(ctx, member, newMember)
	if err != nil {
		glog.Errorln("UpdateServiceMember error", err, "requuid", requuid, member)
		return nil, err
	}

	glog.Infoln("added the cluster config to service member", member.MemberName, member.ServiceUUID, "requuid", requuid)
	return newMember, nil
}

// TODO most code is the same with enableMongoDBAuth, unify it to avoid duplicate code.
func (s *ManageHTTPServer) updateRedisConfigs(ctx context.Context,
	cfg *common.MemberConfig, cfgIndex int, member *common.ServiceMember, requuid string) error {
	// fetch the config file
	cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
	if err != nil {
		glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
		return err
	}

	// if auth is enabled, return
	enableAuth := rediscatalog.NeedToEnableAuth(cfgfile.Content)
	setIP := rediscatalog.NeedToSetClusterAnnounceIP(cfgfile.Content)

	if !enableAuth && !setIP {
		glog.Infoln("auth and cluster-announce-ip are already set in the config file", db.PrintConfigFile(cfgfile), "requuid", requuid, member)
		return nil
	}

	newContent := cfgfile.Content
	if enableAuth {
		// auth is not enabled, enable it
		newContent = rediscatalog.EnableRedisAuth(newContent)
	}
	if setIP {
		// cluster-announce-ip not set, set it
		newContent = rediscatalog.SetClusterAnnounceIP(newContent, member.StaticIP)
	}

	return s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
}

func (s *ManageHTTPServer) updateConsulConfigs(ctx context.Context, serviceUUID string, domain string, requuid string) (serverips []string, err error) {
	// update the consul member address to the assigned static ip in the basic_config.json file
	members, err := s.dbIns.ListServiceMembers(ctx, serviceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "serviceUUID", serviceUUID, "requuid", requuid)
		return nil, err
	}

	serverips = make([]string, len(members))
	memberips := make(map[string]string)
	for i, m := range members {
		memberdns := dns.GenDNSName(m.MemberName, domain)
		memberips[memberdns] = m.StaticIP
		serverips[i] = m.StaticIP
	}

	for _, m := range members {
		err = s.updateConsulMemberConfig(ctx, m, memberips, requuid)
		if err != nil {
			return nil, err
		}
	}

	glog.Infoln("updated ip to consul configs", serviceUUID, "requuid", requuid)
	return serverips, nil
}

// TODO most code is the same with enableMongoDBAuth, unify it to avoid duplicate code.
func (s *ManageHTTPServer) updateConsulMemberConfig(ctx context.Context, member *common.ServiceMember, memberips map[string]string, requuid string) error {
	var cfg *common.MemberConfig
	cfgIndex := -1
	for i, c := range member.Configs {
		if consulcatalog.IsBasicConfigFile(c.FileName) {
			cfg = c
			cfgIndex = i
			break
		}
	}

	// fetch the config file
	cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
	if err != nil {
		glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
		return err
	}

	// replace the original member dns name by member ip
	newContent := consulcatalog.ReplaceMemberName(cfgfile.Content, memberips)

	return s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
}

func (s *ManageHTTPServer) updateMemberConfig(ctx context.Context, member *common.ServiceMember,
	cfgfile *common.ConfigFile, cfgIndex int, newContent string, requuid string) error {
	// create a new config file
	version, err := utils.GetConfigFileVersion(cfgfile.FileID)
	if err != nil {
		glog.Errorln("GetConfigFileVersion error", err, "requuid", requuid, cfgfile)
		return err
	}

	newFileID := utils.GenMemberConfigFileID(member.MemberName, cfgfile.FileName, version+1)
	newcfgfile := db.UpdateConfigFile(cfgfile, newFileID, newContent)

	newcfgfile, err = manage.CreateConfigFile(ctx, s.dbIns, newcfgfile, requuid)
	if err != nil {
		glog.Errorln("CreateConfigFile error", err, "requuid", requuid, db.PrintConfigFile(newcfgfile), member)
		return err
	}

	glog.Infoln("created new config file, requuid", requuid, db.PrintConfigFile(newcfgfile))

	// update serviceMember to point to the new config file
	newConfigs := db.CopyMemberConfigs(member.Configs)
	newConfigs[cfgIndex].FileID = newcfgfile.FileID
	newConfigs[cfgIndex].FileMD5 = newcfgfile.FileMD5

	newMember := db.UpdateServiceMemberConfigs(member, newConfigs)
	err = s.dbIns.UpdateServiceMember(ctx, member, newMember)
	if err != nil {
		glog.Errorln("UpdateServiceMember error", err, "requuid", requuid, member)
		return err
	}

	glog.Infoln("updated member configs in the serviceMember, requuid", requuid, newMember)

	// delete the old config file.
	// TODO add the background gc mechanism to delete the garbage.
	//      the old config file may not be deleted at some conditions.
	//			for example, node crashes right before deleting the config file.
	err = s.dbIns.DeleteConfigFile(ctx, cfgfile.ServiceUUID, cfgfile.FileID)
	if err != nil {
		// simply log an error as this only leaves a garbage
		glog.Errorln("DeleteConfigFile error", err, "requuid", requuid, db.PrintConfigFile(cfgfile))
	} else {
		glog.Infoln("deleted the old config file, requuid", requuid, db.PrintConfigFile(cfgfile))
	}

	return nil
}
