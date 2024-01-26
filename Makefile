all: out/combined_blocklist

generate-blocklists: $(wildcard *.go)
	CGO_ENABLED=0 go build -o generate-blocklists $<

out/combined_blocklist: generate-blocklists
	@mkdir -p $(@D)
	./generate-blocklists

DEPLOY_HOST=alwyzon@www.renehsz.com

.PHONY: deploy
deploy: generate-blocklists
	rsync -rtvzP --delete-after -e ssh generate-blocklists update-blocklists $(DEPLOY_HOST):
	ssh $(DEPLOY_HOST) sudo systemctl restart generate-blocklists

#DEPLOY_PATH=/var/www/renehsz.com/blocklists/
#deploy: out/combined_blocklists
	#rsync -rtvzP --no-perms --delete-after -e ssh out/* $(DEPLOY_HOST):$(DEPLOY_PATH)

