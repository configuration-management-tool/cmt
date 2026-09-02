env {
  NAME  = "api"
  IMAGE = "example/api"
}

hosts_group "local" {
  hosts = ["localhost"]
}

hosts_group "production" {
  hosts = ["api1.example.com", "api2.example.com"]
  env = { DEPLOY_ENV = "prod" }
}

hosts_group "staging" {
  inventory = "curl http://example.com/latest/meta-data/hostname"
}

command "echo" {
  desc = "Echo the resolved env on the local machine"
  run  = "echo hello $NAME from $CMT_HOSTS_GROUP"
}

command "restart" {
  desc   = "Restart example container"
  run    = "sudo docker restart example"
  serial = 2
}

command "build" {
  desc = "Build image"
  run  = "sudo docker build -t image:latest ."
  once = true
}

command "prepare" {
  desc  = "Prepare to upload"
  local = "npm run build"
}

command "upload" {
  desc = "Upload dist files"
  upload {
    src = "./dist"
    dst = "/tmp/"
  }
}

target "deploy" {
  commands = ["build", "prepare", "upload", "restart"]
}
