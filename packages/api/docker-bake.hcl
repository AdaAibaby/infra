// Builds the API server and its DB migrator in parallel.

variable "REGISTRY_PREFIX" {}

variable "COMMIT_SHA" {
  default = ""
}

variable "EXPECTED_MIGRATION_TIMESTAMP" {
  default = ""
}

variable "IMAGE_TAG" {
  default = "latest"
}

group "default" {
  targets = ["api", "db-migrator"]
}

target "api" {
  context    = "."
  dockerfile = "api/Dockerfile"
  platforms  = ["linux/amd64"]
  tags       = concat(["${REGISTRY_PREFIX}/api", "${REGISTRY_PREFIX}/api:${IMAGE_TAG}"], COMMIT_SHA != "" ? ["${REGISTRY_PREFIX}/api:${COMMIT_SHA}"] : [])
  args = {
    COMMIT_SHA                   = COMMIT_SHA
    EXPECTED_MIGRATION_TIMESTAMP = EXPECTED_MIGRATION_TIMESTAMP
  }
}

target "db-migrator" {
  context    = "."
  dockerfile = "db/Dockerfile"
  platforms  = ["linux/amd64"]
  tags       = concat(["${REGISTRY_PREFIX}/db-migrator", "${REGISTRY_PREFIX}/db-migrator:${IMAGE_TAG}"], COMMIT_SHA != "" ? ["${REGISTRY_PREFIX}/db-migrator:${IMAGE_TAG}", "${REGISTRY_PREFIX}/db-migrator:${COMMIT_SHA}"] : [])
}
