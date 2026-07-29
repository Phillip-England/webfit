.PHONY: docker

docker:
	mkdir -p config data public/uploads
	docker build -t webfit . && docker run --rm \
		-p 8787:8787 \
		-v $(CURDIR)/config:/app/config \
		-v $(CURDIR)/data:/app/data \
		-v $(CURDIR)/public/uploads:/app/public/uploads \
		webfit
