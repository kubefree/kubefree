# Image URL to use all building/pushing image targets
IMG ?= aslan-spock-register.qiniu.io/jichangjun/kubefree:v0.1

build:  ## Build manager binary.
	go build ./...
install: 
	go install ./...
linux-build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .	
docker-build: linux-build ## Build docker image with the manager.
	docker build -t ${IMG} .
docker-push: ## Push docker image with the manager.
	docker push ${IMG}
docker-deploy: ## deploy service into kubernetes
	kubectl apply -f config/ 
docker-redeploy: ## redeploy service into kubernetes
	kubectl delete -f config/
	kubectl apply -f config/