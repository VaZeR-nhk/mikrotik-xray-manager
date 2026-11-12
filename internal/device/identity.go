package device

import (
 "crypto/rand"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "regexp"
 "strings"
)

var validHWID=regexp.MustCompile(`^[a-zA-Z0-9=-]{10,64}$`)

type Identity struct { HWID, UserAgent, OS, OSVersion, Model string }

func Load(dataDir, configuredHWID, userAgent, osName, osVersion, model string)(Identity,error){
 id:=Identity{UserAgent:strings.TrimSpace(userAgent),OS:strings.TrimSpace(osName),OSVersion:strings.TrimSpace(osVersion),Model:strings.TrimSpace(model)}
 if id.UserAgent=="" { id.UserAgent="Happ" }; if id.OS=="" { id.OS="Linux" }; if id.Model=="" { id.Model="Debian Xray Gateway" }; if id.OSVersion=="" { id.OSVersion=detectOSVersion() }
 if configuredHWID!="" { if !validHWID.MatchString(configuredHWID){return Identity{},fmt.Errorf("XRAY_MANAGER_HWID must be 10-64 characters: letters, digits, '-' or '='")}; id.HWID=configuredHWID; return id,nil }
 path:=filepath.Join(dataDir,"device.json")
 if b,e:=os.ReadFile(path); e==nil { var saved struct{HWID string `json:"hwid"`}; if e=json.Unmarshal(b,&saved);e!=nil{return Identity{},e}; if !validHWID.MatchString(saved.HWID){return Identity{},fmt.Errorf("stored HWID in %s is invalid",path)}; id.HWID=saved.HWID; return id,nil } else if !os.IsNotExist(e){return Identity{},e}
 random:=make([]byte,16); if _,e:=rand.Read(random);e!=nil{return Identity{},e}; id.HWID="xrm-"+hex.EncodeToString(random)
 if e:=os.MkdirAll(dataDir,0700);e!=nil{return Identity{},e}
 b,e:=json.MarshalIndent(struct{HWID string `json:"hwid"`}{id.HWID},"","  ");if e!=nil{return Identity{},e}
 tmp:=path+".tmp";if e=os.WriteFile(tmp,b,0600);e!=nil{return Identity{},e};if e=os.Rename(tmp,path);e!=nil{_ = os.Remove(tmp);return Identity{},e}
 return id,nil
}
func detectOSVersion()string{b,e:=os.ReadFile("/etc/os-release");if e!=nil{return "unknown"};for _,l:=range strings.Split(string(b),"\n"){if strings.HasPrefix(l,"VERSION_ID="){return strings.Trim(strings.TrimPrefix(l,"VERSION_ID="),`"`)}};return "unknown"}
