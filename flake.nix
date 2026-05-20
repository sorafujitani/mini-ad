{
  description = "mini-ad: hands-on Ad Server learning repo in Go";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        pg = pkgs.postgresql_16;

        # ---------- Redis ----------
        redisApp = pkgs.writeShellApplication {
          name = "mini-ad-redis";
          runtimeInputs = [ pkgs.redis ];
          text = ''
            set -euo pipefail
            REPO_ROOT="''${MINI_AD_DIR:-$PWD}"
            DATA_DIR="$REPO_ROOT/.dev/redis"
            mkdir -p "$DATA_DIR"

            cat > "$DATA_DIR/redis.conf" <<EOF
            port 6379
            bind 127.0.0.1
            dir $DATA_DIR
            dbfilename dump.rdb
            save 60 1
            appendonly no
            EOF

            echo "[mini-ad] starting redis on 127.0.0.1:6379 (data: $DATA_DIR)"
            exec redis-server "$DATA_DIR/redis.conf"
          '';
        };

        # ---------- PostgreSQL init ----------
        pgInitApp = pkgs.writeShellApplication {
          name = "mini-ad-postgres-init";
          runtimeInputs = [ pg ];
          text = ''
            set -euo pipefail
            REPO_ROOT="''${MINI_AD_DIR:-$PWD}"
            PGDATA="''${PGDATA:-$REPO_ROOT/.dev/postgres}"

            if [ -f "$PGDATA/PG_VERSION" ]; then
              echo "[mini-ad] PGDATA already initialized at $PGDATA"
              exit 0
            fi

            mkdir -p "$PGDATA"
            initdb -D "$PGDATA" -U mini -E UTF8 --auth-host=trust --auth-local=trust
            {
              echo "listen_addresses = '127.0.0.1'"
              echo "unix_socket_directories = '$PGDATA'"
              echo "log_statement = 'none'"
            } >> "$PGDATA/postgresql.conf"

            echo "[mini-ad] PGDATA ready at $PGDATA"
          '';
        };

        # ---------- PostgreSQL start ----------
        pgStartApp = pkgs.writeShellApplication {
          name = "mini-ad-postgres";
          runtimeInputs = [ pg ];
          text = ''
            set -euo pipefail
            REPO_ROOT="''${MINI_AD_DIR:-$PWD}"
            PGDATA="''${PGDATA:-$REPO_ROOT/.dev/postgres}"

            if [ ! -f "$PGDATA/PG_VERSION" ]; then
              echo "[mini-ad] PGDATA not initialized. Run: nix run .#postgres-init"
              exit 1
            fi

            echo "[mini-ad] starting postgres on 127.0.0.1:5432 (data: $PGDATA)"
            exec postgres -D "$PGDATA" -k "$PGDATA"
          '';
        };

        # ---------- Create application DB & load schema ----------
        dbCreateApp = pkgs.writeShellApplication {
          name = "mini-ad-db-create";
          runtimeInputs = [ pg ];
          text = ''
            set -euo pipefail
            REPO_ROOT="''${MINI_AD_DIR:-$PWD}"
            PGHOST="''${PGHOST:-$REPO_ROOT/.dev/postgres}"
            PGUSER="''${PGUSER:-mini}"
            DB="''${PGDATABASE:-miniad}"
            SCHEMA="$REPO_ROOT/infra/schema.sql"

            export PGHOST PGUSER

            # Wait briefly for the server socket
            for i in 1 2 3 4 5; do
              if psql -d postgres -tAc "SELECT 1" >/dev/null 2>&1; then
                break
              fi
              echo "[mini-ad] waiting for postgres... ($i)"
              sleep 1
            done

            if psql -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$DB'" | grep -q 1; then
              echo "[mini-ad] database '$DB' already exists"
            else
              createdb "$DB"
              echo "[mini-ad] created database '$DB'"
            fi

            if [ -f "$SCHEMA" ]; then
              psql -d "$DB" -f "$SCHEMA"
              echo "[mini-ad] applied schema $SCHEMA"
            else
              echo "[mini-ad] no schema.sql at $SCHEMA (skip)"
            fi
          '';
        };

        # ---------- Reset everything (destructive) ----------
        resetApp = pkgs.writeShellApplication {
          name = "mini-ad-reset";
          runtimeInputs = [ pkgs.coreutils ];
          text = ''
            set -euo pipefail
            REPO_ROOT="''${MINI_AD_DIR:-$PWD}"
            rm -rf "$REPO_ROOT/.dev"
            echo "[mini-ad] removed $REPO_ROOT/.dev"
          '';
        };

      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_22
            gopls
            gotools
            redis
            pg
            sqlite
            jq
            curl
            httpie
          ];

          shellHook = ''
            export MINI_AD_DIR="$PWD"
            export PGDATA="$PWD/.dev/postgres"
            export PGHOST="$PGDATA"
            export PGUSER="mini"
            export PGDATABASE="miniad"
            export REDIS_URL="redis://127.0.0.1:6379"

            cat <<'EOF'
            [mini-ad] devShell ready

              起動コマンド (別ターミナルで)
                Redis      : nix run .#redis
                Postgres   : nix run .#postgres-init    (初回のみ)
                           : nix run .#postgres &
                           : nix run .#db-create        (DB & schema 投入)
                クリア     : nix run .#reset            (.dev/ を削除)

              Step を動かす
                go run ./steps/step01-hello-ad/

            EOF
          '';
        };

        apps = {
          redis = {
            type = "app";
            program = "${redisApp}/bin/mini-ad-redis";
          };
          postgres-init = {
            type = "app";
            program = "${pgInitApp}/bin/mini-ad-postgres-init";
          };
          postgres = {
            type = "app";
            program = "${pgStartApp}/bin/mini-ad-postgres";
          };
          db-create = {
            type = "app";
            program = "${dbCreateApp}/bin/mini-ad-db-create";
          };
          reset = {
            type = "app";
            program = "${resetApp}/bin/mini-ad-reset";
          };
        };
      });
}
