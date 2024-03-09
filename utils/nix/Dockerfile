FROM nixos/nix:2.17.0

COPY shell.nix /shell.nix

RUN until nix-shell /shell.nix --command "true"; do sleep 300; done

ENTRYPOINT nix-shell
