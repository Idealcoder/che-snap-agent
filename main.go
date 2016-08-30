package main

import (
    "os"
    "time"
    "fmt"
    "bytes"
    "bufio"
    "strings"
    "io/ioutil"
    "net/http"
    "os/exec"
    "github.com/Jeffail/gabs"      //makes parsing JSON easier, as structure does not need to be known 
    "github.com/BurntSushi/toml"   //used for loading/saving config file
    "github.com/gorilla/websocket" //to connect to websockets
    "github.com/howeyc/gopass"     //don't show password on screen when entered
)

//used to hold config settings
type Config struct {
  //from config file
  Endpoint string
  Username string
  Password string
  Workspace_id string

  //populated from Eclipse Che API
  Apikey string
  Machine_id string     //machine id for current workspace docker machine
  Websocket_ws string   //websocket url for workspace websocket     (includes api token
  Websocket_term string //websocket url for machine terminal socket (includes api token)
  Projects []Project    //contains all different projects in workspace 
}

type Project struct {
  Name string //name of project
  Location string //location of source code (git repo)
}

func main() {
  conf := setup()

  for {
    //wait until workspace is running
    for workspaceRunning(conf) == false {
      fmt.Println("Waiting for workspace to start up")
      time.Sleep(10 * time.Second)
    }

    //get workspace info
    conf = workspaceInfo(conf)

    //add commands to workspace
    for _, project := range conf.Projects {
      addCommand(conf,project.Name+":snap&run","bash /projects/script.sh \\\"snap&run\\\" "+project.Name)
      addCommand(conf,project.Name+":snap&upload","bash /projects/script.sh \\\"snap&upload\\\" "+project.Name)
    }

    //connect to workspace_ws and wait for a message
    workspaceListen(conf)
    fmt.Println("Workspace shutdown")
  }
}

//load config file
//check endpoint connectivity
//authencate and get Apikey if needed
func setup() (conf Config){
  confLocation := os.Getenv("HOME")+"/.che-snap-agent.conf"

  if _, err := os.Stat(confLocation); err == nil {
    if _, err := toml.DecodeFile(confLocation, &conf); err != nil {
      panic(err)
    }
  }

  //check endpoint
  if conf.Endpoint == "" {
    fmt.Println("Please enter Eclipse Che Endpoint URL")
    fmt.Printf( "endpoint (e.g. https://beta.codenvy.com/): ")
    fmt.Scanln(&conf.Endpoint)
    fmt.Println("")
  }

  //remove trailing space if there is
  if conf.Endpoint[len(conf.Endpoint)-1:] == "/" {
    conf.Endpoint = conf.Endpoint[:len(conf.Endpoint)-1]
  }

  resp, err := http.Get(conf.Endpoint+"/api/")
  if (err != nil) || (resp.Status != "200 OK"){
    fmt.Println("Cannot connect to Eclipse Che endpoint. Check URL and internet connection")
    os.Exit(1)
  }

  //check if authencation needed
  resp, err = http.Get(conf.Endpoint+"/api/workspace?skipCount=0&maxItems=30")
  if (err !=nil) || (resp.Status !="200 OK"){

    //no username/password provided
    if (conf.Username == "") || (conf.Password == ""){
      fmt.Println("Please enter Eclipse Che login details")
      fmt.Printf("username: ")
      fmt.Scanln(&conf.Username)
      fmt.Printf("password: ")
      pass, _ := gopass.GetPasswd()
      conf.Password = string(pass)
      fmt.Println("")
    }

    message :=`{"username": "`+conf.Username+`","password": "`+conf.Password+`"}`
    res := httpPost(conf.Endpoint+"/api/auth/login",[]byte(message))
    if bytes.Equal(res,[]byte("401 Unauthorized")) {
      fmt.Println("Authentication failed. Please check username and password.")
      os.Exit(1)
    } else {
      //parse for Apikey
      jsonParsed, _ := gabs.ParseJSON(res)
      conf.Apikey = jsonParsed.Path("value").Data().(string)
    }
  }

  //check if workspace exists
  if (conf.Workspace_id==""){
    conf.Workspace_id=promptWorkspace(conf)
  }

  validWorkspaces  := getWorkspaces(conf)
  workspace_exists := false
  for _, workspace := range validWorkspaces {
    if conf.Workspace_id==workspace {
      workspace_exists=true
    }
  }
  if workspace_exists==false {
    fmt.Println("Workspace selected does no exist.")
    os.Exit(1)
  }

  //save config
  buf := new(bytes.Buffer)
  if err := toml.NewEncoder(buf).Encode(conf); err != nil {
    panic(err)
  }
  err = ioutil.WriteFile(os.Getenv("HOME")+"/.che-snap-agent.conf", buf.Bytes(), 0644)
  if err != nil {
    panic(err)
  }
  return conf
}

//ask user which workspace to use
func promptWorkspace(conf Config) (string){
  validWorkspaces :=getWorkspaces(conf)

  if len(validWorkspaces)>2 {
    fmt.Println("Please select which workspace to attach to")
    fmt.Println("   (name)        (id)")

    for i:=0; i < len(validWorkspaces); i+=2 {
      fmt.Printf("%d) "+validWorkspaces[i]+"     "+validWorkspaces[i+1]+"\n",(i+2)/2)
    }

    fmt.Printf("Select: ")
    var input int
    fmt.Scanln(&input)
    fmt.Println("")

    if input < 1 || input > len(validWorkspaces)/2 {
      return promptWorkspace(conf)
    } else {
      return validWorkspaces[(input*2)-1]
    }
  } else {
    fmt.Println("Selected workspace "+validWorkspaces[0]+"to attach to")
    return validWorkspaces[1]
  }
}

//Returns slice of snapcraft workspaces
func getWorkspaces(conf Config) (validWorkspaces []string) {
  url := conf.Endpoint+"/api/workspace?skipCount=0&maxItems=30&token="+conf.Apikey

  body := httpGet(url)
  jsonParsed, _ := gabs.ParseJSON(body)

  workspaces, _ := jsonParsed.Children()
  for _, workspace := range workspaces{
    environments, _ := workspace.Path("config.environments").Children()
    for _, environment := range environments {
      machines, _ := environment.Path("machineConfigs").Children()
      for _, machine := range machines {
        location := machine.Path("source.location").Data().(string)
        if location=="https://raw.githubusercontent.com/Idealcoder/eclipse-che-snapcraft/master/Dockerfile" {
          validWorkspaces = append(validWorkspaces,workspace.Path("config.defaultEnv").Data().(string))
          validWorkspaces = append(validWorkspaces,workspace.Path("id").Data().(string))
        }
      }
    }
  }
  return
}

func build(conf Config){
  var command, projectName, portNumber, repoLocation string
  fmt.Println("Starting Build")

  //open terminal connection
  c, _, err := websocket.DefaultDialer.Dial(conf.Websocket_term, nil)
  if err != nil {
    panic(err)
  }
  defer c.Close()

  done := make(chan struct{})

  //initiate connection
  err = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","data":[170,18]}`))
  if err != nil {
    panic(err)
  }

  //find out command run
  err = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"data","data":"cat /projects/lastcommand.txt \r\n"}`))
  if err != nil {
    panic(err)
  }

  go func() {
    defer c.Close()
    defer close(done)
    for {
      _, message, err := c.ReadMessage()
      if err != nil {
        //panic(err)
        return
      }
  //   fmt.Println(string(message))
     parts := strings.Split(string(message),":")
     if parts[0] == "COMMAND" {
       command = parts[1]
       projectName = parts[2]
       portNumber = parts[3]
     }
    }
  }()

  for command=="" || projectName==""{
    //waiting for command to be recieved over web socket
  }

  //open socket connection on server
  fmt.Println(portNumber)
  err = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"data","data":"stty raw && nc -u localhost `+portNumber+` \r\n"}`))
  if err != nil {
    panic(err)
  }

  fmt.Println("Building project "+projectName)
  for _, project := range conf.Projects {
    if project.Name == projectName {
      repoLocation = project.Location
    }
  }

  //pull down code into /tmp, done seperately so output not shown
  fmt.Println("Pulling code from "+repoLocation)
  getCode := exec.Command("/bin/sh","-c","rm -rf /tmp/"+projectName+" && cd /tmp && git clone "+repoLocation+" "+projectName)
  getCode.Start()
  getCode.Wait() //blocks so code is downloaded before snapcraft runs

  //find out name of snap so it can be run later
  getName := exec.Command("/bin/sh","-c","cat /tmp/"+projectName+"/snapcraft.yaml")
  output, err := getName.StdoutPipe()
  getName.Start()
  in2 := bufio.NewScanner(output)
  in2.Scan()
  snapName := strings.Split(in2.Text(),":")

  var buildString string
  switch command {
    case "snap&run":
      //run build
      fmt.Println("Running snap&run "+projectName+"...")
      //script used to trick snapcraft into thinking it is being run in a terminal, not stdout
      buildString = "echo \"Build starting\"; cd /tmp/"+projectName+"; snapcraft clean; script -q -c snapcraft; rm typescript; snap install *.snap; cd ~; script -q -c /snap/bin/"+strings.TrimSpace(snapName[1])+"; echo \"Build finished\""
    case "snap&upload":
      //run build
      fmt.Println("Running snap&upload "+projectName+"...")
      //script used to trick snapcraft into thinking it is being run in a terminal, not stdout
      buildString = "echo \"Build starting\"; cd /tmp/"+projectName+"; snapcraft clean; script -q -c snapcraft; rm typescript; script -q -c \"snapcraft push *.snap --release=edge\";  echo \"Build finished\""
  }

  cmd := exec.Command("/bin/sh","-c",buildString)
  stdout, err := cmd.StdoutPipe()
  if err != nil {
    panic(err)
  }


  cmd.Start()
  time.Sleep(1 * time.Second)

  reader := bufio.NewReader(stdout)

  for {
    text, _ := reader.ReadBytes(100)
    //send message
    if string(text) != "" {
      jsonObj := gabs.New()
      jsonObj.Set("data", "type")
      jsonObj.SetP(string(text), "data")
      err = c.WriteMessage(websocket.TextMessage, []byte(jsonObj.String()))
      if err != nil {
        panic(err)
      }
    } else {
      time.Sleep(time.Millisecond * 5)
    }
  }

  //cleanly close websocket
  err = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
  c.Close()
  fmt.Println("Build finished")
}

//adds run command to Eclipse Che IDE
//doesn't add another command if one with the same name already exists
func addCommand(conf Config, name string, command string) {
  //check if already command exists
  url :=conf.Endpoint+"/api/workspace/"+conf.Workspace_id+"?skipCount=0&maxItems=30&token="+conf.Apikey
  body := httpGet(url)

  jsonParsed, _ := gabs.ParseJSON(body)
  jsonParsedInner, _ := jsonParsed.Path("config.commands").Children()

  for _, command := range jsonParsedInner {
    if command.Path("name").Data().(string) == name {
      return //command already exists 
    }
  }

  url =conf.Endpoint+"/api/workspace/"+conf.Workspace_id+"/command?token="+conf.Apikey
  message := `{"commandLine": "`+command+`","name": "`+name+`","type": "custom","attributes": {"previewUrl": ""}}`
  httpPost(url,[]byte(message));
  return
}

//listens to workspace websocket
//when run command is clicked in IDE, calls build function
func workspaceListen(conf Config) {
  //message to subscribe to machine:process channel to get notified when a run is executed
  //uuid is ignored, just required for Eclipse Che protocol
  message := `{"uuid":"4B05BF83-A162-4B1A-B181-301FCF00CC61","method":"POST","path":null,"headers":[{"name":"x-everrest-websocket-message-type","value":"subscribe-channel"}],"body":"{\"channel\":\"machine:process:`+conf.Machine_id+`\"}"}`

  c, _, err := websocket.DefaultDialer.Dial(conf.Websocket_ws, nil)
  if err != nil {
    panic(err)
  }
  defer c.Close()

  done := make(chan struct{})

  err = c.WriteMessage(websocket.TextMessage, []byte(message))
  if err != nil {
    panic(err)
  }

  go func() {
    defer c.Close()
    defer close(done)
    for {
      _, message, err := c.ReadMessage()
      if err != nil {
        //panic(err)
        return
      }
      //parse two nested layers of JSON
      jsonParsed, _ := gabs.ParseJSON(message)
      body := jsonParsed.Path("body").Data().(string)
      fmt.Println("Received Eclipse Che Event "+string(message))
      jsonParsedInner,_ := gabs.ParseJSON([]byte(body))
      if jsonParsedInner.Exists("eventType") {
        event := jsonParsedInner.Path("eventType").Data().(string)
        if event == "STARTED" {
          time.Sleep(1 * time.Second) //give Eclipse Che time
          go build(conf)
        }
     }
    }
  }()

  ticker := time.NewTicker(time.Second * 20)
  defer ticker.Stop()

  for {
    select {
      case <-ticker.C:
        //check workspace is still up
        if workspaceRunning(conf)==false {return}
      case <-done:
        return
    }
  }
}

//use Eclipse Che API to get needed information about workspace:
//machine_id, websocket_ws, websocket_term, projects
func workspaceInfo(conf Config) (confNew Config) {
  confNew = conf;
  url :=conf.Endpoint+"/api/workspace/"+conf.Workspace_id+"?skipCount=0&maxItems=30&token="+conf.Apikey
  body := httpGet(url)
  jsonParsed, _ := gabs.ParseJSON(body)

  //involves painfully traversing json to get information needed
  links, _ := jsonParsed.Path("runtime.devMachine.links").Children()
  for _, link := range links {
    if link.Path("rel").Data().(string) == "terminal" {
      confNew.Websocket_term = link.Path("href").Data().(string)
    }
  }

  confNew.Machine_id = jsonParsed.Path("runtime.devMachine.id").Data().(string)


  links2, _ := jsonParsed.Path("config.environments.machineConfigs.links.href").Children()
  //nested 3 arrays deep
  for _, link2 := range links2 {
    lnks2, _  := link2.Children()
    for _, lnk2 := range lnks2 {
      lks2, _ := lnk2.Children()
      for _, lk2 := range lks2 {
        confNew.Websocket_ws = lk2.Data().(string)+"?token="+conf.Apikey
      }
    }
  }

  jsonProjects, _ := jsonParsed.Path("config.projects").Children()
  for _, jsonProject := range jsonProjects {
    var project Project
    project.Name = jsonProject.Path("name").Data().(string)
    project.Location = jsonProject.Path("source.location").Data().(string)
    confNew.Projects = append(confNew.Projects,project)
  }

  return confNew
}

//check if workspace is running using Eclipse Che API
func workspaceRunning(conf Config) (bool){
  url :=conf.Endpoint+"/api/workspace/"+conf.Workspace_id+"?skipCount=0&maxItems=30&token="+conf.Apikey
  body := httpGet(url)
  jsonParsed, _ := gabs.ParseJSON(body)

  if jsonParsed.Path("status").Data().(string) == "RUNNING" {
    return true
  } else {
    return false
  }
}

//helpers for http requests
func httpGet(url string) ([]byte){
  resp, err := http.Get(url)
  if err !=nil {
    panic(err)
  }

  defer resp.Body.Close()
  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
    panic(err)
  }
  if resp.Status != "200 OK" {
    panic("HTTP Get url "+url+" returned code "+resp.Status)
  }

  return body
}

func httpPost(url string, message []byte) ([]byte) {
  req, err := http.NewRequest("POST", url, bytes.NewBuffer(message))
  req.Header.Set("Content-Type", "application/json")

  client := &http.Client{}
  resp, err := client.Do(req)
  if err != nil {
    panic(err)
  }
  defer resp.Body.Close()
  body, _ := ioutil.ReadAll(resp.Body)

  if (resp.Status != "200 OK") && (resp.Status!= "401 Unauthorized") {
    panic("HTTP Post url "+url+" returned code "+resp.Status)
  }
  if (resp.Status=="401 Unauthorized"){ body=[]byte(resp.Status) }
  return body
}
