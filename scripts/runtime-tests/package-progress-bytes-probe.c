/* SPDX-License-Identifier: MIT
 * Real package ABI over synthetic layouts and sparse files only. No account,
 * game, network or cloud. In particular, a ready nonempty installation must
 * be usable by a title's installedBytes / totalBytes progress calculation. */
#define COBJMACROS
#define INITGUID
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "xpackage.h"
#include "xtaskqueue.h"
#include "xasyncprovider.h"

static unsigned checks, failures;
static IXPackageImpl4 *package;
static IXThreadingImpl *threading;
static WCHAR root[MAX_PATH];
static const char *identifier = "12345678123456789abcdef012345678";
static const WCHAR *layout_name = L"layout_12345678-1234-5678-9abc-def012345678.xml";
static const char *config = "<Game><Identity Name=\"Nexus.Progress\" Publisher=\"CN=Probe\" Version=\"1.2.3.4\"/><StoreId>B00000000001</StoreId></Game>";
static const char *layout = "<Package>"
    "<Chunk Id=\"Registration\"><FileGroup SourcePath=\".\" Include=\"MicrosoftGame.config\"/></Chunk>"
    "<Chunk Id=\"7\" Languages=\"en-US\" Tags=\"base\"><FileGroup SourcePath=\".\" Include=\"base.bin\"/></Chunk>"
    "<Chunk Id=\"9\" Tags=\"extra\"><FileGroup SourcePath=\".\" Include=\"optional.bin\"/></Chunk>"
    "<Chunk Id=\"11\" Tags=\"empty\"><FileGroup SourcePath=\".\" Include=\"empty.bin\"/></Chunk>"
    "<Chunk Id=\"12\" Tags=\"big\"><FileGroup SourcePath=\".\" Include=\"big.bin\"/></Chunk>"
    "<Chunk Id=\"13\" Tags=\"base\"><FileGroup SourcePath=\".\" Include=\"BASE.bin\"/>"
    "<FileGroup SourcePath=\".\" Include=\"base.bin\"/></Chunk></Package>";
#define CHECK(name,expr) do { ++checks; if (!(expr)) { ++failures; printf("FAIL %s line=%d\n",name,__LINE__); } } while(0)
static void write_file(const WCHAR *name, const char *text)
{
    WCHAR full[MAX_PATH]; DWORD written; HANDLE file;
    swprintf(full, MAX_PATH, L"%ls\\%ls", root, name);
    file=CreateFileW(full,GENERIC_WRITE,FILE_SHARE_READ,NULL,CREATE_ALWAYS,0,NULL);
    CHECK("fixture-open",file!=INVALID_HANDLE_VALUE);
    if(file==INVALID_HANDLE_VALUE)return;
    CHECK("fixture-write",WriteFile(file,text,strlen(text),&written,NULL)&&written==strlen(text)); CloseHandle(file);
}
static XPackageInstallationMonitorHandle create(UINT count, XPackageChunkSelector *selectors, XTaskQueueHandle queue)
{
    XPackageInstallationMonitorHandle monitor=NULL;
    CHECK("monitor-create",IXPackageImpl4_XPackageCreateInstallationMonitor(package,identifier,count,selectors,10,queue,&monitor)==S_OK&&monitor);
    return monitor;
}
static XPackageInstallationProgress read_progress(XPackageInstallationMonitorHandle monitor)
{
    XPackageInstallationProgress value; memset(&value,0xcc,sizeof(value));
    IXPackageImpl4_XPackageGetInstallationProgress(package,monitor,&value); return value;
}
static void ready(XPackageInstallationMonitorHandle monitor, UINT64 bytes)
{
    XPackageInstallationProgress value=read_progress(monitor);
    CHECK("ready-byte-total",value.totalBytes==bytes);
    CHECK("ready-installed-bytes",value.installedBytes==bytes);
    CHECK("ready-launch-bytes",value.launchBytes==bytes);
    CHECK("ready-flags",value.completed&&value.launchable);
    CHECK("nonempty-title-divisor",!bytes||value.totalBytes>0);
    if(bytes&&value.totalBytes) CHECK("title-percentage",100*value.installedBytes/value.totalBytes==100);
}
static void unknown(XPackageInstallationMonitorHandle monitor)
{
    XPackageInstallationProgress value=read_progress(monitor);
    CHECK("unknown-total-sentinel",value.totalBytes==~(UINT64)0);
    CHECK("unknown-no-invented-work",!value.installedBytes&&!value.launchBytes&&!value.completed&&!value.launchable);
}
struct callback_state { UINT calls; UINT64 bytes; };
static void WINAPI callback(void *context,XPackageInstallationMonitorHandle monitor)
{
    struct callback_state *state=context; ++state->calls; ready(monitor,state->bytes);
}
static void run_tests(void)
{
    HMODULE dll=LoadLibraryW(L"xgameruntime.dll");
    HRESULT (WINAPI *query)(const GUID *,REFIID,void **)=dll?(void *)GetProcAddress(dll,"QueryApiImpl"):NULL;
    XPackageChunkSelector selectors[3]={{0}};
    XPackageInstallationMonitorHandle monitor;
    UINT64 reg=strlen(config),big=((UINT64)5<<30)+123;
    WCHAR full[MAX_PATH]; LARGE_INTEGER size; HANDLE file;
    CHECK("runtime-interface",query!=NULL); if(!query)return;
    CHECK("package-interface",query(&CLSID_XPackageImpl,&IID_IXPackageImpl4,(void **)&package)==S_OK&&package);
    CHECK("queue-interface",query(&CLSID_XThreadingImpl,&IID_IXThreadingImpl,(void **)&threading)==S_OK&&threading);
    if(!package||!threading)return;
    GetModuleFileNameW(NULL,root,MAX_PATH); *wcsrchr(root,'\\')=0;
    for(UINT i=0;i<3;i++)selectors[i].type=XPackageChunkSelectorType_Chunk;
    selectors[0].chunkId=7;
    monitor=create(1,selectors,NULL); ready(monitor,reg+9); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    selectors[0].chunkId=9;
    monitor=create(1,selectors,NULL); unknown(monitor); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    selectors[0].chunkId=999;
    monitor=create(1,selectors,NULL); unknown(monitor); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    selectors[0].chunkId=11;
    monitor=create(1,selectors,NULL); ready(monitor,reg); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    selectors[0].chunkId=7;selectors[1].chunkId=13;selectors[2].chunkId=7;
    monitor=create(3,selectors,NULL); ready(monitor,reg+9); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    selectors[0].type=XPackageChunkSelectorType_Tag;selectors[0].tag="base";
    monitor=create(1,selectors,NULL); ready(monitor,reg+9); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    monitor=create(0,NULL,NULL); unknown(monitor); IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    swprintf(full,MAX_PATH,L"%ls\\big.bin",root); file=CreateFileW(full,GENERIC_WRITE,0,NULL,CREATE_ALWAYS,0,NULL);
    size.QuadPart=big;
    CHECK("sparse-file-open",file!=INVALID_HANDLE_VALUE);
    if(file!=INVALID_HANDLE_VALUE) {CHECK("sparse-size",SetFilePointerEx(file,size,NULL,FILE_BEGIN)&&SetEndOfFile(file));CloseHandle(file);}
    selectors[0].type=XPackageChunkSelectorType_Chunk; selectors[0].chunkId=12;
    monitor=create(1,selectors,NULL);ready(monitor,reg+big);IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    write_file(L"optional.bin","extra");
    monitor=create(0,NULL,NULL);ready(monitor,reg+9+5+big);IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    {
        XTaskQueueHandle queue=NULL;XTaskQueueRegistrationToken token={0};
        struct callback_state state={0,reg+9}; selectors[0].chunkId=7;
        CHECK("manual-queue",IXThreadingImpl_XTaskQueueCreate(threading,XTaskQueueDispatchMode_Manual,XTaskQueueDispatchMode_Manual,&queue)==S_OK);
        monitor=create(1,selectors,queue);
        CHECK("register",IXPackageImpl4_XPackageRegisterInstallationProgressChanged(package,monitor,&state,callback,&token)==S_OK);
        CHECK("initial-not-inline",state.calls==0);
        CHECK("initial-dispatch",IXThreadingImpl_XTaskQueueDispatch(threading,queue,XTaskQueuePort_Completion,1000)&&state.calls==1);
        write_file(L"base.bin","larger synthetic file");state.bytes=reg+strlen("larger synthetic file");
        CHECK("resize-refresh",IXPackageImpl4_XPackageUpdateInstallationMonitor(package,monitor));
        CHECK("byte-only-change-notifies",IXThreadingImpl_XTaskQueueDispatch(threading,queue,XTaskQueuePort_Completion,1000)&&state.calls==2);
        CHECK("unchanged-refresh",IXPackageImpl4_XPackageUpdateInstallationMonitor(package,monitor));
        CHECK("unchanged-no-duplicate",!IXThreadingImpl_XTaskQueueDispatch(threading,queue,XTaskQueuePort_Completion,0));
        CHECK("unregister",IXPackageImpl4_XPackageUnregisterInstallationProgressChanged(package,monitor,token,TRUE));
        IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);IXThreadingImpl_XTaskQueueCloseHandle(threading,queue);
    }
    write_file(layout_name,"<Package><Chunk Id=\"7\"><FileGroup SourcePath=\".\" Include=\"base.bin\"/><FileGroup SourcePath=\".\" Include=\"BASE.bin\"/></Chunk></Package>");
    monitor=create(1,selectors,NULL);ready(monitor,strlen("larger synthetic file"));IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    write_file(layout_name,"<Package><Chunk Id=\"7\"><FileGroup SourcePath=\".\" Include=\"empty.bin\"/></Chunk></Package>");
    monitor=create(1,selectors,NULL);ready(monitor,0);IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    write_file(layout_name,layout);
    monitor=create(1,selectors,NULL);ready(monitor,reg+strlen("larger synthetic file"));
    write_file(layout_name,"not package metadata");
    CHECK("invalid-refresh-fails",!IXPackageImpl4_XPackageUpdateInstallationMonitor(package,monitor));
    unknown(monitor);IXPackageImpl4_XPackageCloseInstallationMonitorHandle(package,monitor);
    write_file(layout_name,layout);
    IXPackageImpl4_Release(package);IXThreadingImpl_Release(threading);FreeLibrary(dll);
}
int main(int argc,char **argv)
{
    if(argc==2&&!strcmp(argv[1],"--child"))run_tests();
    else
    {
        WCHAR temp[MAX_PATH],self[MAX_PATH],exe[MAX_PATH],command[MAX_PATH+30];
        STARTUPINFOW startup={0};PROCESS_INFORMATION child={0};DWORD code=99;
        startup.cb=sizeof(startup);
        CHECK("temp",GetTempPathW(MAX_PATH,temp));swprintf(root,MAX_PATH,L"%lsnexus-bytes-%lu-%lu",temp,GetCurrentProcessId(),GetTickCount());
        CHECK("private-fixture",CreateDirectoryW(root,NULL));
        write_file(L"MicrosoftGame.config",config);
        write_file(L"appxmanifest.xml","<Package><Identity Name=\"Nexus.Progress\" Publisher=\"CN=Probe\" Version=\"1.2.3.4\"/><Applications/></Package>");
        write_file(layout_name,layout);write_file(L"base.bin","synthetic");write_file(L"empty.bin","");
        CHECK("self",GetModuleFileNameW(NULL,self,MAX_PATH));swprintf(exe,MAX_PATH,L"%ls\\probe.exe",root);
        CHECK("copy",CopyFileW(self,exe,TRUE));swprintf(command,MAX_PATH+30,L"\"%ls\" --child",exe);
        CHECK("child",CreateProcessW(exe,command,NULL,NULL,FALSE,0,NULL,NULL,&startup,&child));
        if(child.hProcess){DWORD wait=WaitForSingleObject(child.hProcess,15000);CHECK("child-finished",wait==WAIT_OBJECT_0);if(wait!=WAIT_OBJECT_0){TerminateProcess(child.hProcess,77);WaitForSingleObject(child.hProcess,1000);}GetExitCodeProcess(child.hProcess,&code);CHECK("child-result",code==0);CloseHandle(child.hProcess);CloseHandle(child.hThread);}
    }
    printf("CHECKS %u FAILURES %u\n",checks,failures);return failures?1:0;
}
