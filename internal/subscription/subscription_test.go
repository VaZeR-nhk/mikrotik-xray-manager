package subscription

import (
 "net/http"
 "net/http/httptest"
 "testing"
)

func TestFetchAndParseSendsRemnawaveHeaders(t *testing.T) {
 var got http.Header
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  got=r.Header.Clone()
  _,_=w.Write([]byte("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&type=tcp#gateway"))
 }))
 defer server.Close()
 old:=client
 client=server.Client()
 defer func(){client=old}()
 _,err:=FetchAndParse(server.URL,RequestIdentity{UserAgent:"Xray-Manager/test",HWID:"xrm-0123456789abcdef0123456789abcdef",DeviceOS:"Linux",OSVersion:"13",DeviceModel:"Debian Gateway"})
 if err!=nil {t.Fatal(err)}
 for k,want:=range map[string]string{"User-Agent":"Xray-Manager/test","x-hwid":"xrm-0123456789abcdef0123456789abcdef","x-device-os":"Linux","x-ver-os":"13","x-device-model":"Debian Gateway"} {
  if got.Get(k)!=want {t.Errorf("%s = %q, want %q",k,got.Get(k),want)}
 }
}

func TestFetchAndParseRejectsEmptyBody(t *testing.T) {
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){}))
 defer server.Close()
 old:=client;client=server.Client();defer func(){client=old}()
 if _,err:=FetchAndParse(server.URL,RequestIdentity{});err==nil {t.Fatal("expected empty subscription error")}
}

func TestFetchAndParseReportsHWIDLimit(t *testing.T) {
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("x-hwid-max-devices-reached","true")
  _,_=w.Write([]byte("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&type=tcp#gateway"))
 }))
 defer server.Close()
 old:=client;client=server.Client();defer func(){client=old}()
 if _,err:=FetchAndParse(server.URL,RequestIdentity{HWID:"xrm-0123456789abcdef"});err==nil {t.Fatal("expected device limit error")}
}
