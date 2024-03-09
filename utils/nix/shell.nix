let
  # nixos-23.11 go1.21
  pkgs = import (fetchTarball "https://github.com/NixOS/nixpkgs/archive/2be119add7b37dc535da2dd4cba68e2cf8d1517e.tar.gz"){};
in pkgs.mkShell {
  buildInputs = [
    pkgs.go
    pkgs.nodejs-18_x
    pkgs.ffmpeg-headless
    pkgs.golangci-lint
    pkgs.gotools
    pkgs.gofumpt
    pkgs.gopls
    pkgs.revive
    pkgs.shfmt
    pkgs.shellcheck
  ];
}
