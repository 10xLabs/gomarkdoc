build:
	go clean
	./gentmpl.sh templates templates
	cd cmd/gomarkdoc && go install 
