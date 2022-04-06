FROM ghcr.io/edgelesssys/ego-dev:latest

RUN git clone https://github.com/obscuronet/obscuro-playground
## todo - joel - remove this line as a follow-up once branch is merged
RUN cd obscuro-playground && git checkout joel/enclave_docker_file
RUN cd obscuro-playground/go/obscuronode/enclave/main && ego-go build && ego sign main

ENV OE_SIMULATION=1
ENTRYPOINT ["ego", "run", "obscuro-playground/go/obscuronode/enclave/main/main"]
EXPOSE 11000
