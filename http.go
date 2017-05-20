package git

/*
#include <git2.h>
#include <git2/sys/transport.h>

typedef struct {
    git_smart_subtransport parent;
    void *ptr;
} managed_smart_subtransport;

typedef struct {
    git_smart_subtransport_stream parent;
    void *ptr;
} managed_smart_subtransport_stream;

int _go_git_transport_register(const char *scheme);
int _go_git_transport_smart(git_transport **out, git_remote *owner);
void _go_git_setup_smart_subtransport(managed_smart_subtransport *t, void *ptr);
void _go_git_setup_smart_subtransport_stream(managed_smart_subtransport_stream *t, void *ptr);
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"runtime"
	"unsafe"
)

type SmartService int

const (
	SmartServiceUploadpackLs  = C.GIT_SERVICE_UPLOADPACK_LS
	SmartServiceUploadpack    = C.GIT_SERVICE_UPLOADPACK
	SmartServiceReceivepackLs = C.GIT_SERVICE_RECEIVEPACK_LS
	SmartServiceReceivepack   = C.GIT_SERVICE_RECEIVEPACK
)

type SmartSubtransport interface {
	Action(url string, action SmartService) (SmartSubtransportStream, error)
	Close() error
	Free()
}

type SmartSubtransportStream interface {
	Read(buf []byte) (int, error)
	Write(buf []byte) error
	Free()
}

func RegisterManagedHttp() error {
	httpStr := C.CString("http")
	defer C.free(unsafe.Pointer(httpStr))

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ret := C._go_git_transport_register(httpStr)
	if ret != 0 {
		return MakeGitError(ret)
	}

	return nil
}

func RegisterManagedHttps() error {
	httpsStr := C.CString("https")
	defer C.free(unsafe.Pointer(httpsStr))

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ret := C._go_git_transport_register(httpsStr)
	if ret != 0 {
		return MakeGitError(ret)
	}

	return nil
}

type ManagedTransport struct {
	owner *C.git_transport

	client *http.Client
}

func (self *ManagedTransport) Action(url string, action SmartService) (SmartSubtransportStream, error) {
	if err := self.ensureClient(); err != nil {
		return nil, err
	}

	var req *http.Request
	var err error
	switch action {
	case SmartServiceUploadpackLs:
		req, err = http.NewRequest("GET", url+"/info/refs?service=git-upload-pack", nil)

	case SmartServiceUploadpack:
		req, err = http.NewRequest("POST", url+"/git-upload-pack", nil)
		if err != nil {
			break
		}

		req.Header["Content-Type"] = []string{"application/x-git-upload-pack-request"}

	case SmartServiceReceivepackLs:
		req, err = http.NewRequest("GET", url+"/info/refs?service=git-receive-pack", nil)

	case SmartServiceReceivepack:
		req, err = http.NewRequest("POST", url+"/info/refs?service=git-upload-pack", nil)
		if err != nil {
			break
		}

		req.Header["Content-Type"] = []string{"application/x-git-receive-pack-request"}
	default:
		err = errors.New("unknown action")
	}

	if err != nil {
		return nil, err
	}

	req.Header["User-Agent"] = []string{"git/2.0 (git2go)"}
	return newManagedHttpStream(self, req), nil
}

func (self *ManagedTransport) Close() error {
	self.client = nil
	return nil
}

func (self *ManagedTransport) Free() {
}

func (self *ManagedTransport) ensureClient() error {
	if self.client != nil {
		return nil
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	self.client = &http.Client{Transport: transport}

	return nil
}

type ManagedHttpStream struct {
	owner       *ManagedTransport
	req         *http.Request
	resp        *http.Response
	postBuffer  bytes.Buffer
	sentRequest bool
}

func newManagedHttpStream(owner *ManagedTransport, req *http.Request) *ManagedHttpStream {
	return &ManagedHttpStream{
		owner: owner,
		req:   req,
	}
}

func (self *ManagedHttpStream) Read(buf []byte) (int, error) {
	if !self.sentRequest {
		if err := self.sendRequest(); err != nil {
			return 0, err
		}
	}

	return self.resp.Body.Read(buf)
}

func (self *ManagedHttpStream) Write(buf []byte) error {
	// We write it all into a buffer and send it off when the transport asks
	// us to read.
	self.postBuffer.Write(buf)
	return nil
}

func (self *ManagedHttpStream) Free() {
	self.resp.Body.Close()
}

func (self *ManagedHttpStream) sendRequest() error {
	var resp *http.Response
	var err error
	var userName string
	var password string
	for {
		req := &http.Request{
			Method:        self.req.Method,
			URL:           self.req.URL,
			Header:        self.req.Header,
			Body:          ioutil.NopCloser(&self.postBuffer),
			ContentLength: int64(self.postBuffer.Len()),
		}

		req.SetBasicAuth(userName, password)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			var cred *C.git_cred

			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ret := C.git_transport_smart_credentials(&cred, self.owner.owner, nil, C.GIT_CREDTYPE_USERPASS_PLAINTEXT)

			if ret != 0 {
				return MakeGitError(ret)
			}

			if cred.credtype != C.GIT_CREDTYPE_USERPASS_PLAINTEXT {
				C.git_cred_free(cred)
				return fmt.Errorf("Unexpected credential type %d", cred.credtype)
			}
			ptCred := (*C.git_cred_userpass_plaintext)(unsafe.Pointer(cred))
			userName = C.GoString(ptCred.username)
			password = C.GoString(ptCred.password)
			C.git_cred_free(cred)

			continue
		}

		// Any other error we treat as a hard error and punt back to the caller
		resp.Body.Close()
		return fmt.Errorf("Unhandled HTTP error %s", resp.Status)
	}

	self.sentRequest = true
	self.resp = resp
	return nil
}

func setLibgit2Error(err error) C.int {
	cstr := C.CString(err.Error())
	defer C.free(unsafe.Pointer(cstr))
	C.giterr_set_str(C.GITERR_NET, cstr)

	if gitErr, ok := err.(*GitError); ok {
		return C.int(gitErr.Code)
	}

	return -1
}

//export httpAction
func httpAction(out **C.git_smart_subtransport_stream, t *C.git_smart_subtransport, url *C.char, action C.git_smart_service_t) C.int {
	transport, err := getSmartSubtransportInterface(t)
	if err != nil {
		return setLibgit2Error(err)
	}

	managed, err := transport.Action(C.GoString(url), SmartService(action))
	if err != nil {
		return setLibgit2Error(err)
	}

	stream := C.calloc(1, C.size_t(unsafe.Sizeof(C.managed_smart_subtransport_stream{})))
	managedPtr := pointerHandles.Track(managed)
	C._go_git_setup_smart_subtransport_stream(stream, managedPtr)

	*out = (*C.git_smart_subtransport_stream)(stream)
	return 0
}

//export httpClose
func httpClose(t *C.git_smart_subtransport) C.int {
	transport, err := getSmartSubtransportInterface(t)
	if err != nil {
		return setLibgit2Error(err)
	}

	if err := transport.Close(); err != nil {
		return setLibgit2Error(err)
	}

	return 0
}

//export httpFree
func httpFree(transport *C.git_smart_subtransport) {
	wrapperPtr := (*C.managed_smart_subtransport)(unsafe.Pointer(transport))
	pointerHandles.Untrack(wrapperPtr.ptr)
}

var errNoSmartSubtransport = errors.New("passed object does not implement SmartSubtransport")

func getSmartSubtransportInterface(_t *C.git_smart_subtransport) (SmartSubtransport, error) {
	wrapperPtr := (*C.managed_smart_subtransport)(unsafe.Pointer(_t))

	transport, ok := pointerHandles.Get(wrapperPtr.ptr).(SmartSubtransport)
	if !ok {
		return nil, errNoSmartSubtransport
	}

	return transport, nil
}

//export httpTransportCb
func httpTransportCb(out **C.git_transport, owner *C.git_remote, param unsafe.Pointer) C.int {
	return C._go_git_transport_smart(out, owner)
}

//export httpSmartSubtransportCb
func httpSmartSubtransportCb(out **C.git_smart_subtransport, owner *C.git_transport, param unsafe.Pointer) C.int {
	if out == nil {
		return -1
	}

	transport := C.calloc(1, C.size_t(unsafe.Sizeof(C.managed_smart_subtransport{})))
	managed := &ManagedTransport{owner: owner}
	managedPtr := pointerHandles.Track(managed)
	C._go_git_setup_smart_subtransport(transport, managedPtr)

	*out = (*C.git_smart_subtransport)(transport)
	return 0
}

var errNoSmartSubtransportStream = errors.New("passed object does not implement SmartSubtransportStream")

func getSmartSubtransportStreamInterface(_s *C.git_smart_subtransport_stream) (SmartSubtransportStream, error) {
	wrapperPtr := (*C.managed_smart_subtransport_stream)(unsafe.Pointer(_s))

	transport, ok := pointerHandles.Get(wrapperPtr.ptr).(SmartSubtransportStream)
	if !ok {
		return nil, errNoSmartSubtransportStream
	}

	return transport, nil
}

//export smartSubtransportRead
func smartSubtransportRead(s *C.git_smart_subtransport_stream, data *C.char, l C.size_t, read *C.size_t) C.int {
	stream, err := getSmartSubtransportStreamInterface(s)
	if err != nil {
		return setLibgit2Error(err)
	}

	var p []byte
	header := (*reflect.SliceHeader)(unsafe.Pointer(&p))
	header.Cap = int(l)
	header.Len = int(l)
	header.Data = uintptr(unsafe.Pointer(data))

	n, err := stream.Read(p)
	if err != nil {
		if err == io.EOF {
			*read = C.size_t(0)
			return 0
		}

		setLibgit2Error(err)
		return -1
	}

	*read = C.size_t(n)
	return 0
}

//export smartSubtransportWrite
func smartSubtransportWrite(s *C.git_smart_subtransport_stream, data unsafe.Pointer, l C.size_t) C.int {
	stream, err := getSmartSubtransportStreamInterface(s)
	if err != nil {
		return setLibgit2Error(err)
	}

	var p []byte
	header := (*reflect.SliceHeader)(unsafe.Pointer(&p))
	header.Cap = int(l)
	header.Len = int(l)
	header.Data = uintptr(data)

	if err := stream.Write(p); err != nil {
		return setLibgit2Error(err)
	}

	return 0
}

//export smartSubtransportFree
func smartSubtransportFree(s *C.git_smart_subtransport_stream) {
}
