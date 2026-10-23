package main

import (
 "bytes"
 "io"
 "net/http"
 "os"
 "strings"
 "testing"
)

func TestValidRepo(t *testing.T) {
 for input, want := range map[string]bool{"owner/repo":true,"owner/":false,"/repo":false,"repo":false,"a/b/c":false,"a/b?x":false,"a/..":false} {
  if got := validRepo(input); got != want { t.Errorf("validRepo(%q) = %v, want %v",input,got,want) }
 }
}
type transportFunc func(*http.Request) (*http.Response, error)
func (f transportFunc) RoundTrip(r *http.Request) (*http.Response,error) { return f(r) }

func TestAssetHTTP(t *testing.T) {
 old := httpClient
 t.Cleanup(func(){httpClient=old})
 for _, status := range []int{200,401,404,500} {
  httpClient=&http.Client{Transport:transportFunc(func(r *http.Request)(*http.Response,error){
   if r.Header.Get("Accept")!="application/octet-stream" { t.Error("missing binary accept header") }
   return &http.Response{StatusCode:status,Body:io.NopCloser(strings.NewReader("binary")),Header:make(http.Header)},nil
  })}
  var dst bytes.Buffer
  err:=downloadAsset("https://api.github.com/repos/a/b/releases/assets/1",&dst)
  if (err==nil)!=(status==200) {t.Errorf("status %d: %v",status,err)}
  if status!=200 && dst.Len()!=0 {t.Error("downloaded error body as asset")}
 }
}
func TestUploadAsset(t *testing.T) {
 old:=httpClient
 t.Cleanup(func(){httpClient=old})
 f,err:=os.CreateTemp(t.TempDir(),"asset")
 if err!=nil {t.Fatal(err)}
 defer f.Close()
 if _,err=f.WriteString("binary");err!=nil {t.Fatal(err)}
 for _,status:=range []int{201,422,500} {
  if _,err=f.Seek(0,0);err!=nil {t.Fatal(err)}
  httpClient=&http.Client{Transport:transportFunc(func(r *http.Request)(*http.Response,error){
   if r.URL.Host!="uploads.github.com" || r.URL.Path!="/repos/a/b/releases/123/assets" || r.URL.Query().Get("name")!="file +.zip" {t.Errorf("wrong upload URL: %s",r.URL)}
   if r.Method!="POST" || r.ContentLength!=6 {t.Error("wrong upload request")}
   if _,err:=io.ReadAll(r.Body);err!=nil {t.Error(err)}
   if err:=r.Body.Close();err!=nil {t.Error(err)}
   return &http.Response{StatusCode:status,Body:io.NopCloser(strings.NewReader("{}")),Header:make(http.Header)},nil
  })}
  err=uploadAsset("a/b",123,"file +.zip",f)
  if (err==nil)!=(status==201) {t.Errorf("status %d: %v",status,err)}
 }
}
func TestSameAsset(t *testing.T) {
 if sameAsset(asset{Size:4},asset{Size:4}) {t.Fatal("size alone cannot prove identical contents")}
 if sameAsset(asset{Digest:"sha256:a"},asset{Digest:"sha256:b"}) {t.Fatal("different digests")}
 if !sameAsset(asset{Digest:"sha256:a"},asset{Digest:"sha256:a"}) {t.Fatal("matching digests")}
}
