package device

import (
 "os"
 "path/filepath"
 "regexp"
 "testing"
)

func TestLoadPersistsStableHWID(t *testing.T) {
 dir:=t.TempDir()
 first,err:=Load(dir,"","Xray-Manager/test","Linux","13","Debian Gateway");if err!=nil{t.Fatal(err)}
 if !regexp.MustCompile(`^[a-zA-Z0-9=-]{10,64}$`).MatchString(first.HWID){t.Fatalf("invalid HWID %q",first.HWID)}
 second,err:=Load(dir,"","Xray-Manager/test","Linux","13","Debian Gateway");if err!=nil{t.Fatal(err)}
 if first.HWID!=second.HWID{t.Fatalf("HWID changed: %q != %q",first.HWID,second.HWID)}
 info,err:=os.Stat(filepath.Join(dir,"device.json"));if err!=nil{t.Fatal(err)}
 if info.Mode().Perm()!=0600{t.Fatalf("device.json mode %o, want 600",info.Mode().Perm())}
}
func TestLoadRejectsInvalidConfiguredHWID(t *testing.T){if _,err:=Load(t.TempDir(),"bad!","","","","");err==nil{t.Fatal("expected invalid HWID error")}}
func TestConfiguredHWIDOverridesPersisted(t *testing.T){id,err:=Load(t.TempDir(),"manual-device-123","","","","");if err!=nil{t.Fatal(err)};if id.HWID!="manual-device-123"{t.Fatalf("got %q",id.HWID)}}
