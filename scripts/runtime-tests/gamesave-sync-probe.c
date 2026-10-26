/* SPDX-License-Identifier: MIT
 * Compile the actual save implementation with a closed synthetic transport.
 * No runtime user, real credentials, network or production save directories. */
#define INITGUID
#include "private.h"
#include "util.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
char *msaAppId, *packageFamilyName = "Nexus.SaveFixture";
UINT32 titleId;
BOOLEAN fullTrust = TRUE;
IXUserImpl *x_user_impl;
static unsigned checks, failures, requests, blob_gets, writes;
static BOOL fail_second;
static BOOL fail_promote;
static unsigned metadata_case;
static unsigned listing_case;
static const char *remote_time="2026-10-22T12:00:00Z";
#define CHECK(name,expr) do { ++checks; if (!(expr)) { ++failures; printf("FAIL %s line=%d\n",name,__LINE__); } } while(0)
HRESULT WINAPI QueryApiImpl(const GUID *clsid,REFIID iid,void **out) { *out=NULL;return E_NOTIMPL; }
UINT64 xuser_get_xuid(XUserHandle user) { return 42; }
HRESULT xuser_device_authorization(XUserHandle user,const char *relying,char **auth) { *auth=strdup("synthetic-not-a-token");return *auth?S_OK:E_OUTOFMEMORY; }
HRESULT encode_base64(UINT32 size,const BYTE *data,UINT32 capacity,char *out,BOOLEAN pad) { return E_ACCESSDENIED; }
HRESULT http_request_raw(const WCHAR *method,const WCHAR *url,const WCHAR *headers,const void *body,DWORD count,DWORD *status,BYTE **out,SIZE_T *size)
{ *status=403;*out=NULL;*size=0;return E_ACCESSDENIED; }
HRESULT xuser_signed_request(XUserHandle user,const char *method,const char *url,const char *auth,const char *headers,const void *body,DWORD count,DWORD *status,BYTE **out,SIZE_T *size)
{
    const char *response=NULL;char listing[512];
    ++requests;*out=NULL;*size=0;*status=500;
    if(strcmp(method,"GET")){++writes;return E_ACCESSDENIED;}
    if(!strcmp(url,"https://fixture.invalid"))
    {
        snprintf(listing,sizeof(listing),"{\"blobs\":[{\"fileName\":\"Save,savedgame\",\"displayName\":\"Save\",\"clientFileTime\":\"%s\"}]}",remote_time);
        response=listing;
        if(listing_case)
        {
            const char *cases[]={NULL,"{\"blobs\":[]", "{\"blobs\":[17]}", "{\"blobs\":[{\"fileName\":\"Save,savedgame\"},]}",
                "{\"blobs\":[{\"fileName\":\"Save,savedgame\"}{\"fileName\":\"Other,savedgame\"}]}","{\"blobs\":[{}]}",
                "{\"blobs\":[{\"fileName\":\"Save,savedgame\"},{\"fileName\":\"Save,savedgame\"}]}","{\"blobs\":[]}garbage"};
            response=cases[listing_case];
        }
    }
    else if(!strcmp(url,"https://fixture.invalid/Save,savedgame"))
    {
        const char *cases[]={"{\"atoms\":{\"one\":\"atom-A\",\"two\":\"atom-B\"}}",
            "{\"atoms\":{\"one\":\"atom-A\",\"two\":\"atom-B\"", "{\"atoms\":{\"one\":\"atom-A\",\"one\":\"atom-B\"}}",
            "{\"atoms\":{\"one\":17}}", "{\"atoms\":{\"one\":\"\"}}", "{\"atoms\":{\"one\":\"atom-A\",}}"};
        response=cases[metadata_case];
    }
    else if(!strcmp(url,"https://fixture.invalid/atom-A")){++blob_gets;response="remote-A";}
    else if(!strcmp(url,"https://fixture.invalid/atom-B")){++blob_gets;if(fail_second){*status=503;response="unavailable";}else response="remote-B";}
    else return E_ACCESSDENIED;
    if(*status!=503)*status=200;
    *size=strlen(response);*out=malloc(*size+1);if(!*out)return E_OUTOFMEMORY;memcpy(*out,response,*size+1);return S_OK;
}
static BOOL WINAPI test_move(const WCHAR *from,const WCHAR *to,DWORD flags)
{
    if(fail_promote&&!(flags&MOVEFILE_REPLACE_EXISTING)&&wcsstr(from,L"\\.incoming-"))
    {SetLastError(ERROR_ACCESS_DENIED);return FALSE;}
    return MoveFileExW(from,to,flags);
}
#define MoveFileExW test_move
#include "xgamesave.c"
#undef MoveFileExW
static void seed(struct XGameSaveProvider *provider,BOOL dirty)
{
    struct container_state state={0};struct blob_entry *entry;char file[2048],dir[2048];
    state.name=strdup("Save");state.display=strdup("Save");state.dirty=dirty;state.time=parse_iso_time("2026-10-20T12:00:00Z");strcpy(state.remote,"2026-10-20T12:00:00Z");
    container_dir(provider,"Save",dir,sizeof(dir));create_directories(dir);
    entry=add_blob(&state,"one");entry->size=7;strcpy(entry->atom,"old-A");blob_path(provider,"Save","one",file,sizeof(file));CHECK("seed-one",write_file(file,"local-A",7)==S_OK);
    entry=add_blob(&state,"two");entry->size=7;strcpy(entry->atom,"old-B");blob_path(provider,"Save","two",file,sizeof(file));CHECK("seed-two",write_file(file,"local-B",7)==S_OK);
    CHECK("seed-state",save_state(provider,&state)==S_OK);free_state(&state);
}
static void preserved(struct XGameSaveProvider *provider,BOOL dirty)
{
    struct container_state state;char file[2048];BYTE *bytes=NULL;SIZE_T size=0;HRESULT hr;
    hr=load_state(provider,"Save",&state);CHECK("original-index-preserved",hr==S_OK);
    if(SUCCEEDED(hr)){CHECK("original-metadata",state.dirty==dirty&&!strcmp(state.remote,"2026-10-20T12:00:00Z")&&state.count==2);}
    free_state(&state);
    blob_path(provider,"Save","one",file,sizeof(file));hr=read_file(file,&bytes,&size);CHECK("original-one-preserved",hr==S_OK&&size==7&&!memcmp(bytes,"local-A",7));free(bytes);bytes=NULL;
    blob_path(provider,"Save","two",file,sizeof(file));hr=read_file(file,&bytes,&size);CHECK("original-two-preserved",hr==S_OK&&size==7&&!memcmp(bytes,"local-B",7));free(bytes);
}
static BOOL count_local(struct XGameSaveProvider *provider,const char *name,void *context)
{
    unsigned *count=context; ++*count; CHECK("only-live-container-enumerated",!strcmp(name,"Save"));return TRUE;
}
static void run_provider(BOOL files)
{
    WCHAR temp[MAX_PATH];char path[1024];struct XGameSaveProvider provider={0};HRESULT hr;
    printf("CASE provider %s\n",files?"files":"blobs");
    provider.files=files;
    CHECK("temp",GetTempPathW(MAX_PATH,temp)>0);
    CHECK("utf8-temp",WideCharToMultiByte(CP_UTF8,0,temp,-1,path,sizeof(path),NULL,NULL)>0);
    snprintf(provider.root,sizeof(provider.root),"%snexus-sync-%lu-%I64u-%u",path,GetCurrentProcessId(),GetTickCount64(),files);
    strcpy(provider.base,"https://fixture.invalid");provider.quota=SAVE_QUOTA;provider.user=(void *)1;
    create_directories(provider.root);
    seed(&provider,FALSE);fail_second=TRUE;
    hr=cloud_download(&provider,"Save","Save",remote_time);CHECK("failed-download-reported",FAILED(hr));preserved(&provider,FALSE);
    fail_second=FALSE;
    for(metadata_case=1;metadata_case<=5;metadata_case++)
    {
        seed(&provider,FALSE);hr=cloud_download(&provider,"Save","Save",remote_time);
        CHECK("malformed-download-rejected",FAILED(hr));preserved(&provider,FALSE);
    }
    metadata_case=0;seed(&provider,FALSE);fail_promote=TRUE;
    hr=cloud_download(&provider,"Save","Save",remote_time);CHECK("promotion-error-reported",FAILED(hr));preserved(&provider,FALSE);fail_promote=FALSE;
    SetEnvironmentVariableA("XODUS_SAVE_SYNC","auto");
    for(listing_case=1;listing_case<=7;listing_case++)
    {
        printf("CASE invalid listing %u\n",listing_case);
        seed(&provider,FALSE);hr=cloud_sync(&provider);CHECK("invalid-listing-fails-closed",FAILED(hr));preserved(&provider,FALSE);
        free(provider.auth);provider.auth=NULL;
    }
    listing_case=0;
    seed(&provider,TRUE);fail_second=FALSE;blob_gets=writes=0;SetEnvironmentVariableA("XODUS_SAVE_SYNC","auto");
    hr=cloud_sync(&provider);CHECK("dirty-conflict-reported",hr==E_GS_CONTAINER_NOT_IN_SYNC);CHECK("conflict-no-payload-or-upload",!blob_gets&&!writes);preserved(&provider,TRUE);free(provider.auth);provider.auth=NULL;
    seed(&provider,FALSE);remote_time="2026-10-20T12:00:00Z";blob_gets=writes=0;
    hr=cloud_sync(&provider);CHECK("unchanged-sync",hr==S_OK&&!blob_gets&&!writes);preserved(&provider,FALSE);free(provider.auth);provider.auth=NULL;
    seed(&provider,TRUE);remote_time="2026-10-22T12:00:00Z";SetEnvironmentVariableA("XODUS_SAVE_SYNC","download");
    hr=cloud_sync(&provider);CHECK("explicit-download",hr==S_OK&&!writes);
    {
        struct container_state state;CHECK("download-state",load_state(&provider,"Save",&state)==S_OK);
        CHECK("download-metadata",!state.dirty&&state.count==2&&!strcmp(state.remote,remote_time));free_state(&state);
    }
    {
        char pattern[2048],file[2048],*key=files?files_cache_key("one"):hex_encode("one");WIN32_FIND_DATAA data;HANDLE find;BYTE *bytes=NULL;SIZE_T size=0;unsigned count=0;BOOL found=FALSE;
        snprintf(pattern,sizeof(pattern),"%s\\*",provider.root);
        find=save_find_first(pattern,&data);
        if(find!=INVALID_HANDLE_VALUE)
        {
            do{
                if(strncmp(data.cFileName,".recovery-",10)&&!strstr(data.cFileName,".backup-"))continue;
                snprintf(file,sizeof(file),"%s\\%s\\b_%s",provider.root,data.cFileName,key);
                if(SUCCEEDED(read_file(file,&bytes,&size))&&size==7&&!memcmp(bytes,"local-A",7))found=TRUE;
                free(bytes);bytes=NULL;
            }while(FindNextFileA(find,&data));FindClose(find);
        }
        free(key);CHECK("recovery-copy-kept",found);
        enumerate_local(&provider,count_local,&count);CHECK("recovery-not-in-cloud-set",count==1);
    }
    free(provider.auth);CHECK("no-network-writes",writes==0);
}
int main(void)
{
    run_provider(FALSE);run_provider(TRUE);
    printf("CHECKS %u FAILURES %u\n",checks,failures);return failures?1:0;
}
