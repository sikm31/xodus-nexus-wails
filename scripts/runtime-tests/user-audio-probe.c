/* SPDX-License-Identifier: MIT
 * Actual audio implementation and real task queues, closed test-only MMDevice
 * source. No account, real endpoint changes, audio streams or network. */
#define INITGUID
#include "private.h"
#include <mmdeviceapi.h>
#include <stdio.h>
WINE_DEFAULT_DEBUG_CHANNEL(gdkc);
static LONG checks, failures, allocations, source_count, enum_refs, device_refs;
static LONG fail_create, fail_register, fail_allocate, fail_malloc;
static BOOL signed_in = TRUE;
static BOOL missing_endpoint;
static BOOL real_source;
static HRESULT (WINAPI *native_query)(REFCLSID, REFIID, void **);
static IXThreadingImpl *threading;
static SRWLOCK source_lock = SRWLOCK_INIT;
static IMMNotificationClient *clients[128];
static HANDLE create_entered,create_resume;
static const WCHAR render_id[] = L"render-\x017e\x0165", capture_id[] = L"capture-\x010d";
#define CHECK(n,v) do { InterlockedIncrement(&checks); if (!(v)) { InterlockedIncrement(&failures); printf("FAIL %s line=%d\n",n,__LINE__); } } while(0)
HRESULT WINAPI QueryApiImpl(REFCLSID clsid, REFIID iid, void **out) { return native_query(clsid,iid,out); }
static BOOL audio_current_user(XUserLocalId *user) { if(signed_in)user->value=1;return signed_in; }
static void *tracked_calloc(size_t n,size_t size) {
    if(InterlockedExchange(&fail_allocate,0))return NULL;
    void *p=calloc(n,size);if(p)InterlockedIncrement(&allocations);return p;
}
static void *tracked_malloc(size_t size) { if(InterlockedExchange(&fail_malloc,0))return NULL;void *p=malloc(size);if(p)InterlockedIncrement(&allocations);return p; }
static void tracked_free(void *p) { if(p)InterlockedDecrement(&allocations);free(p); }
static ULONG WINAPI enum_release(IMMDeviceEnumerator *e) { return InterlockedDecrement(&enum_refs); }
static ULONG WINAPI device_release(IMMDevice *d) { return InterlockedDecrement(&device_refs); }
static HRESULT WINAPI render_get_id(IMMDevice *d,WCHAR **out) {
    *out=CoTaskMemAlloc(sizeof(render_id));if(!*out)return E_OUTOFMEMORY;memcpy(*out,render_id,sizeof(render_id));return S_OK;
}
static HRESULT WINAPI capture_get_id(IMMDevice *d,WCHAR **out) {
    *out=CoTaskMemAlloc(sizeof(capture_id));if(!*out)return E_OUTOFMEMORY;memcpy(*out,capture_id,sizeof(capture_id));return S_OK;
}
static const IMMDeviceVtbl render_vtable={.Release=device_release,.GetId=render_get_id};
static const IMMDeviceVtbl capture_vtable={.Release=device_release,.GetId=capture_get_id};
static IMMDevice render={&render_vtable},capture={&capture_vtable};
static HRESULT WINAPI get_default(IMMDeviceEnumerator *e,EDataFlow flow,ERole role,IMMDevice **out) {
    CHECK("communications-role",role==eCommunications);*out=NULL;if(missing_endpoint)return E_NOTFOUND;
    InterlockedIncrement(&device_refs);*out=flow==eRender?&render:&capture;return S_OK;
}
static HRESULT WINAPI register_client(IMMDeviceEnumerator *e,IMMNotificationClient *client) {
    if(fail_register)return E_ACCESSDENIED;
    AcquireSRWLockExclusive(&source_lock);
    unsigned i;for(i=0;i<128&&clients[i];i++);
    if(i<128){clients[i]=client;InterlockedIncrement(&source_count);}
    ReleaseSRWLockExclusive(&source_lock);return i<128?S_OK:E_OUTOFMEMORY;
}
static HRESULT WINAPI unregister_client(IMMDeviceEnumerator *e,IMMNotificationClient *client) {
    AcquireSRWLockExclusive(&source_lock);
    unsigned i;for(i=0;i<128&&clients[i]!=client;i++);
    if(i<128){clients[i]=NULL;InterlockedDecrement(&source_count);}
    ReleaseSRWLockExclusive(&source_lock);return i<128?S_OK:E_NOTFOUND;
}
static const IMMDeviceEnumeratorVtbl enumerator_vtable={.Release=enum_release,.GetDefaultAudioEndpoint=get_default,
    .RegisterEndpointNotificationCallback=register_client,.UnregisterEndpointNotificationCallback=unregister_client};
static IMMDeviceEnumerator enumerator={&enumerator_vtable};
static HRESULT WINAPI create_enumerator(REFCLSID clsid,IUnknown *outer,DWORD clsctx,REFIID iid,void **out) {
    if(real_source)return CoCreateInstance(clsid,outer,clsctx,iid,out);
    if(create_entered){SetEvent(create_entered);WaitForSingleObject(create_resume,30000);}
    *out=NULL;if(fail_create)return REGDB_E_CLASSNOTREG;
    if(!IsEqualGUID(clsid,&CLSID_MMDeviceEnumerator)||!IsEqualGUID(iid,&IID_IMMDeviceEnumerator))return E_NOINTERFACE;
    InterlockedIncrement(&enum_refs);*out=&enumerator;return S_OK;
}
#define CoCreateInstance create_enumerator
#define calloc tracked_calloc
#define malloc tracked_malloc
#define free tracked_free
#include "xuser_audio.h"
#undef free
#undef malloc
#undef calloc
#undef CoCreateInstance
static void fire(EDataFlow flow,ERole role,const WCHAR *id) {
    IMMNotificationClient *copy[128];unsigned count=0;
    AcquireSRWLockShared(&source_lock);
    for(unsigned i=0;i<128;i++)if(clients[i]){copy[count++]=clients[i];IMMNotificationClient_AddRef(clients[i]);}
    ReleaseSRWLockShared(&source_lock);
    for(unsigned i=0;i<count;i++){IMMNotificationClient_OnDefaultDeviceChanged(copy[i],flow,role,id);IMMNotificationClient_Release(copy[i]);}
}
struct callback_state { LONG called;XTaskQueueRegistrationToken token;BOOL self_remove;HANDLE entered,resume;WCHAR id[64];BOOL null_id; };
static void WINAPI changed(void *arg,XUserLocalId user,XUserDefaultAudioEndpointKind kind,const WCHAR *id) {
    struct callback_state *s=arg;CHECK("event-user",user.value==1);CHECK("event-kind",kind==0||kind==1);
    s->null_id=id==NULL;if(id)lstrcpynW(s->id,id,64);InterlockedIncrement(&s->called);
    if(s->entered) {SetEvent(s->entered);CHECK("callback-resumed",WaitForSingleObject(s->resume,5000)==WAIT_OBJECT_0);}
    if(s->self_remove) CHECK("self-unregister-does-not-wait-for-itself",!audio_unregister(s->token,TRUE));
}
static XTaskQueueHandle queue(XTaskQueueDispatchMode mode) {
    XTaskQueueHandle q=NULL;CHECK("create-queue",IXThreadingImpl_XTaskQueueCreate(threading,mode,mode,&q)==S_OK);return q;
}
static void drained(void) {
    for(unsigned i=0;i<5000&&(allocations||source_count||enum_refs||device_refs);i++)Sleep(1);
    CHECK("no-source-clients",!source_count);CHECK("no-enumerator-leaks",!enum_refs);
    CHECK("no-device-leaks",!device_refs);CHECK("no-registration-or-event-leaks",!allocations);
}
static void getters(void) {
    XUserLocalId user={1},unknown={2};WCHAR out[64];SIZE_T used=99;
    CHECK("invalid-output",audio_get_default(user,0,64,NULL,&used)==E_INVALIDARG&&used==0);
    CHECK("invalid-kind",audio_get_default(user,42,64,out,&used)==E_INVALIDARG);
    CHECK("unknown-user",audio_get_default(unknown,0,64,out,&used)==E_GAMEUSER_USER_NOT_FOUND);
    signed_in=FALSE;CHECK("no-user",audio_get_default(user,0,64,out,&used)==E_GAMEUSER_USER_NOT_FOUND);signed_in=TRUE;
    out[1]=L'X';CHECK("small-buffer",audio_get_default(user,0,1,out,&used)==HRESULT_FROM_WIN32(ERROR_INSUFFICIENT_BUFFER));
    CHECK("size-includes-terminator",used==ARRAY_SIZE(render_id)&&!out[0]&&out[1]==L'X');
    CHECK("unicode-render",audio_get_default(user,0,64,out,&used)==S_OK&&!wcscmp(out,render_id));
    CHECK("capture",audio_get_default(user,1,64,out,NULL)==S_OK&&!wcscmp(out,capture_id));
    missing_endpoint=TRUE;CHECK("no-endpoint-is-not-fabricated",audio_get_default(user,0,64,out,&used)==E_NOTFOUND&&!out[0]&&!used);missing_endpoint=FALSE;
    fail_create=TRUE;CHECK("honest-source-failure",audio_get_default(user,0,64,out,&used)==REGDB_E_CLASSNOTREG);fail_create=FALSE;drained();
}
static void contracts(void) {
    struct callback_state s={0};XTaskQueueHandle q=queue(XTaskQueueDispatchMode_Manual);XTaskQueueRegistrationToken token={99};
    CHECK("null-token",audio_register(q,&s,changed,NULL)==E_INVALIDARG);
    CHECK("null-callback",audio_register(q,&s,NULL,&token)==E_INVALIDARG&&!token.token);
    CHECK("zero-token-invalid",!audio_unregister(token,TRUE));token.token=~(UINT64)0;
    CHECK("unknown-token-invalid",!audio_unregister(token,TRUE));
    XTaskQueueHandle saved=NULL;CHECK("save-process-queue",IXThreadingImpl_XTaskQueueGetCurrentProcessTaskQueue(threading,&saved));
    IXThreadingImpl_XTaskQueueSetCurrentProcessTaskQueue(threading,NULL);
    CHECK("missing-default-queue",audio_register(NULL,&s,changed,&token)==HRESULT_FROM_WIN32(ERROR_NO_TASK_QUEUE)&&!token.token);
    IXThreadingImpl_XTaskQueueSetCurrentProcessTaskQueue(threading,saved);IXThreadingImpl_XTaskQueueCloseHandle(threading,saved);
    CHECK("default-queue-registration",audio_register(NULL,&s,changed,&token)==S_OK&&token.token);
    CHECK("default-queue-unregister",audio_unregister(token,TRUE));drained();
    fail_allocate=1;CHECK("allocation-error",audio_register(q,&s,changed,&token)==E_OUTOFMEMORY&&!token.token);
    fail_create=TRUE;CHECK("registration-source-fails",audio_register(q,&s,changed,&token)==REGDB_E_CLASSNOTREG&&!token.token);fail_create=FALSE;drained();
    fail_register=TRUE;CHECK("native-registration-fails",audio_register(q,&s,changed,&token)==E_ACCESSDENIED&&!token.token);fail_register=FALSE;drained();
    CHECK("register",audio_register(q,&s,changed,&s.token)==S_OK&&s.token.token);
    CHECK("no-invented-initial-event",!IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0)&&!s.called);
    fail_allocate=1;fire(eRender,eCommunications,L"allocation-error");
    fail_malloc=1;fire(eRender,eCommunications,L"copy-error");
    CHECK("event-allocation-failures-suppressed",!IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0)&&!s.called);
    WCHAR copied[]=L"endpoint-copy";fire(eRender,eCommunications,copied);copied[0]=L'X';
    CHECK("completion-not-work-port",!IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Work,0));
    CHECK("manual-event",IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0)&&s.called==1&&!wcscmp(s.id,L"endpoint-copy"));
    fire(eCapture,eCommunications,NULL);CHECK("endpoint-removal",IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0)&&s.null_id);
    fire(eRender,eConsole,L"ignore");signed_in=FALSE;fire(eRender,eCommunications,L"ignore");signed_in=TRUE;
    CHECK("irrelevant-events-ignored",!IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0)&&s.called==2);
    fire(eRender,eCommunications,L"canceled");CHECK("unregister-queued-without-manual-dispatch",audio_unregister(s.token,TRUE));
    IXThreadingImpl_XTaskQueueDispatch(threading,q,XTaskQueuePort_Completion,0);CHECK("queued-event-suppressed",s.called==2);
    CHECK("idempotent-unregister",audio_unregister(s.token,TRUE));IXThreadingImpl_XTaskQueueCloseHandle(threading,q);drained();
    q=queue(XTaskQueueDispatchMode_Immediate);s=(struct callback_state){.self_remove=TRUE};
    CHECK("register-immediate",audio_register(q,&s,changed,&s.token)==S_OK);fire(eRender,eCommunications,L"inline");
    CHECK("inline-self-removal",s.called==1);IXThreadingImpl_XTaskQueueCloseHandle(threading,q);drained();
    q=queue(XTaskQueueDispatchMode_Manual);s=(struct callback_state){0};
    CHECK("caller-queue-retained",audio_register(q,&s,changed,&s.token)==S_OK);
    IXThreadingImpl_XTaskQueueCloseHandle(threading,q);CHECK("unregister-after-caller-close",audio_unregister(s.token,TRUE));drained();
    q=queue(XTaskQueueDispatchMode_Manual);s=(struct callback_state){0};
    CHECK("terminated-queue-registration",audio_register(q,&s,changed,&s.token)==S_OK);
    CHECK("terminate-queue",IXThreadingImpl_XTaskQueueTerminate(threading,q,FALSE,NULL,NULL)==S_OK);
    fire(eRender,eCommunications,L"terminated");CHECK("unregister-terminated",audio_unregister(s.token,TRUE));
    IXThreadingImpl_XTaskQueueCloseHandle(threading,q);drained();
    q=queue(XTaskQueueDispatchMode_Manual);UINT64 previous=audio_next_token;audio_next_token=~(UINT64)0;
    CHECK("token-overflow-fails-closed",audio_register(q,&s,changed,&token)==HRESULT_FROM_WIN32(ERROR_ARITHMETIC_OVERFLOW)&&!token.token);
    audio_next_token=previous;IXThreadingImpl_XTaskQueueCloseHandle(threading,q);drained();
}
static void WINAPI stress_changed(void *context,XUserLocalId user,XUserDefaultAudioEndpointKind kind,const WCHAR *id) {
    CHECK("parallel-event-user",user.value==1);InterlockedIncrement((LONG*)context);
}
static DWORD WINAPI stress(void *arg) {
    LONG count=0;
    for(unsigned i=0;i<12;i++) {
        XTaskQueueHandle q=queue(XTaskQueueDispatchMode_Immediate);XTaskQueueRegistrationToken token={0};
        CHECK("parallel-register",audio_register(q,&count,stress_changed,&token)==S_OK&&token.token);
        fire(eRender,eCommunications,L"parallel");
        CHECK("parallel-unregister",audio_unregister(token,TRUE));IXThreadingImpl_XTaskQueueCloseHandle(threading,q);
    }
    return 0;
}
static void parallel(void) {
    HANDLE workers[4];
    for(unsigned i=0;i<4;i++)workers[i]=CreateThread(NULL,0,stress,NULL,0,NULL);
    CHECK("parallel-workers",WaitForMultipleObjects(4,workers,TRUE,15000)==WAIT_OBJECT_0);
    for(unsigned i=0;i<4;i++)CloseHandle(workers[i]);
    drained();
}
static void startup_timeout(void) {
    XTaskQueueHandle q=queue(XTaskQueueDispatchMode_Manual);XTaskQueueRegistrationToken token={99};
    create_entered=CreateEventW(NULL,TRUE,FALSE,NULL);create_resume=CreateEventW(NULL,TRUE,FALSE,NULL);
    ULONGLONG start=GetTickCount64();HRESULT hr=audio_register(q,NULL,changed,&token);
    CHECK("startup-timeout",hr==HRESULT_FROM_WIN32(ERROR_TIMEOUT)&&!token.token);
    CHECK("startup-timeout-bounded",GetTickCount64()-start>=14000&&GetTickCount64()-start<30000);
    SetEvent(create_resume);IXThreadingImpl_XTaskQueueCloseHandle(threading,q);drained();
    CloseHandle(create_entered);CloseHandle(create_resume);create_entered=create_resume=NULL;
}
static void native_abi(void) {
    IXUserDeviceImpl2 *device=NULL;XUserLocalId missing={0};WCHAR id[64];SIZE_T used=99;
    XTaskQueueHandle q=queue(XTaskQueueDispatchMode_Manual);XTaskQueueRegistrationToken token={99};struct callback_state s={0};
    CHECK("native-audio-interface",native_query(&CLSID_XUserDeviceImpl,&IID_IXUserDeviceImpl2,(void**)&device)==S_OK);
    if(device) {
        CHECK("native-invalid-output",IXUserDeviceImpl2_XUserGetDefaultAudioEndpointUtf16(device,missing,0,64,NULL,&used)==E_INVALIDARG&&used==0);
        CHECK("native-no-account-does-not-invent-user",IXUserDeviceImpl2_XUserGetDefaultAudioEndpointUtf16(device,missing,0,64,id,&used)==E_GAMEUSER_USER_NOT_FOUND);
        CHECK("native-null-callback",IXUserDeviceImpl2_XUserRegisterForDefaultAudioEndpointUtf16Changed(device,q,NULL,NULL,&token)==E_INVALIDARG&&!token.token);
        HRESULT hr=IXUserDeviceImpl2_XUserRegisterForDefaultAudioEndpointUtf16Changed(device,q,&s,changed,&token);
        printf("NATIVE AUDIO REGISTRATION %08lx\n",hr);
        CHECK("native-registration-not-stub",hr!=E_NOTIMPL);
        if(SUCCEEDED(hr)) {
            CHECK("native-token",token.token!=0);
            CHECK("native-unregister",IXUserDeviceImpl2_XUserUnregisterForDefaultAudioEndpointUtf16Changed(device,token,TRUE));
        } else CHECK("native-failed-registration-has-no-token",!token.token);
        IXUserDeviceImpl2_Release(device);
    }
    IXThreadingImpl_XTaskQueueCloseHandle(threading,q);
}
static void real_endpoints(void) {
    /* Synthetic user only in this EXE. Read actual communication defaults and
     * confirm the returned identifier resolves, without opening an audio stream. */
    HRESULT initialized=CoInitializeEx(NULL,COINIT_MULTITHREADED);
    CHECK("real-com-initialized",SUCCEEDED(initialized));
    if(FAILED(initialized))return;
    IMMDeviceEnumerator *e=NULL;HRESULT source=CoCreateInstance(&CLSID_MMDeviceEnumerator,NULL,CLSCTX_INPROC_SERVER,&IID_IMMDeviceEnumerator,(void**)&e);
    real_source=TRUE;
    for(unsigned kind=0;kind<2;kind++) {
        WCHAR id[32768];SIZE_T used=99;XUserLocalId user={1};IMMDevice *expected=NULL,*resolved=NULL;WCHAR *expected_id=NULL;
        HRESULT actual=audio_get_default(user,kind,ARRAY_SIZE(id),id,&used);
        HRESULT hr=FAILED(source)?source:IMMDeviceEnumerator_GetDefaultAudioEndpoint(e,kind?eCapture:eRender,eCommunications,&expected);
        printf("REAL AUDIO ENDPOINT kind=%u status=%08lx\n",kind,actual);
        CHECK("real-endpoint-status",actual==hr);
        if(SUCCEEDED(hr)) {
            CHECK("real-endpoint-id",IMMDevice_GetId(expected,&expected_id)==S_OK&&expected_id&&!wcscmp(id,expected_id)&&used==wcslen(id)+1);
            CHECK("real-endpoint-resolves",IMMDeviceEnumerator_GetDevice(e,id,&resolved)==S_OK&&resolved);
        } else CHECK("real-endpoint-failure-empty",!id[0]&&!used);
        CoTaskMemFree(expected_id);if(resolved)IMMDevice_Release(resolved);if(expected)IMMDevice_Release(expected);
    }
    real_source=FALSE;if(e)IMMDeviceEnumerator_Release(e);CoUninitialize();
}
struct unregistration { XTaskQueueRegistrationToken token;BOOL wait,result; };
static DWORD WINAPI remove_thread(void *arg) { struct unregistration *u=arg;u->result=audio_unregister(u->token,u->wait);return 0; }
static void inflight(BOOL wait) {
    struct callback_state s={0};XTaskQueueHandle q=queue(XTaskQueueDispatchMode_ThreadPool);
    struct unregistration u={0};HANDLE thread;
    s.entered=CreateEventW(NULL,TRUE,FALSE,NULL);s.resume=CreateEventW(NULL,TRUE,FALSE,NULL);
    CHECK("register-running",audio_register(q,&s,changed,&s.token)==S_OK);fire(eRender,eCommunications,L"blocked");
    CHECK("callback-running",WaitForSingleObject(s.entered,5000)==WAIT_OBJECT_0);
    u.token=s.token;u.wait=wait;thread=CreateThread(NULL,0,remove_thread,&u,0,NULL);
    CHECK("unregister-wait-policy",WaitForSingleObject(thread,50)==(wait?WAIT_TIMEOUT:WAIT_OBJECT_0));
    if(!wait)CHECK("nonwaiting-reports-running",!u.result);
    SetEvent(s.resume);CHECK("unregister-completes",WaitForSingleObject(thread,5000)==WAIT_OBJECT_0);
    if(wait)CHECK("waiting-reports-drained",u.result);
    CloseHandle(thread);for(unsigned i=0;i<5000&&allocations;i++)Sleep(1);
    CHECK("running-token-retired",audio_unregister(s.token,TRUE));
    IXThreadingImpl_XTaskQueueCloseHandle(threading,q);CloseHandle(s.entered);CloseHandle(s.resume);drained();
}
int main(void) {
    HMODULE dll=LoadLibraryW(L"xgameruntime.dll");native_query=dll?(void*)GetProcAddress(dll,"QueryApiImpl"):NULL;
    CHECK("native-threading",native_query&&native_query(&CLSID_XThreadingImpl,&IID_IXThreadingImpl,(void**)&threading)==S_OK);
    if(!threading)return 1;
    getters();contracts();inflight(FALSE);inflight(TRUE);parallel();startup_timeout();native_abi();real_endpoints();
    IXThreadingImpl_Release(threading);printf("CHECKS %ld FAILURES %ld\n",checks,failures);return failures?1:0;
}
