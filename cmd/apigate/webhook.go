package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/golang/glog"
	v1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

type Webhook struct {
	server *http.Server
}

// Webhook Server parameters
type WhSvrParameters struct {
	port           int    // webhook server port
	certFile       string // path to the x509 certificate for https
	keyFile        string // path to the x509 private key matching `CertFile`
	sidecarCfgFile string // path to sidecar injector configuration file
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []patchOperation) {
	for key, value := range added {
		if target == nil || target[key] == "" {
			target = map[string]string{}
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + key,
				Value: value,
			})
		}
	}
	return patch
}

func (wh *Webhook) mutate(ar *v1.AdmissionReview) *v1.AdmissionResponse {
	req := ar.Request

	var (
		resourceName string
	)

	glog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, resourceName, req.UID, req.Operation, req.UserInfo)

	var result *metav1.Status

	return &v1.AdmissionResponse{
		Allowed: true,
		Result:  result,
	}
}

func (wh *Webhook) validate(ar *v1.AdmissionReview) *v1.AdmissionResponse {

	req := ar.Request

	glog.Infof("validate request, Group=%v, Version=%v, Kind=%s, Namespace=%v, Name=%v, UID=%v, patchOperation=%v UserInfo=%v",
		req.Kind.Group, req.Kind.Version, req.Kind.Kind, req.Namespace, req.Name, req.UID, req.Operation, req.UserInfo)

	glog.Infof("validate request, Object=%v, oldObject=%v, options=%v",
		string(req.Object.Raw), string(req.OldObject.Raw), string(req.Options.Raw))

	// OPERATIONs 是 CREATE/UPDATE/DELETE/CONNECT
	// TODO: 为什么GET/LIST请求不触发Webhook事件呢？

	var response = &v1.AdmissionResponse{
		Allowed: true,
	}

	// 排除系统产生事件或者Cluster级别操作
	if strings.HasPrefix(req.UserInfo.Username, "system:") || req.Namespace == "" {
		return response
	}

	// TODO: 如果是删除NS，那我们去更新NS，会有啥表现呢？

	return &v1.AdmissionResponse{
		Allowed: true,
	}
}

// Serve method for webhook server
func (wh *Webhook) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		glog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	var admissionResponse *v1.AdmissionResponse
	ar := v1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		glog.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		glog.Infof("URL:%v", r.URL.Path)
		if r.URL.Path == "/mutate" {
			admissionResponse = wh.mutate(&ar)
		} else if r.URL.Path == "/validate" {
			admissionResponse = wh.validate(&ar)
		}
	}

	admissionReview := v1.AdmissionReview{}

	// refer: https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/#response
	admissionReview.APIVersion = ar.APIVersion
	admissionReview.Kind = ar.Kind

	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		glog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	glog.Infof("Ready to write response: %v", string(resp))
	if _, err := w.Write(resp); err != nil {
		glog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
