# Plane server deployment

The deployment entry point is now at the repository root:

- [docker-compose.yml](../../docker-compose.yml): published server images
- [configure.sh](../../configure.sh): generate deployment configuration
- [DEPLOYMENT.md](../../DEPLOYMENT.md): deployment, HTTPS, verification and upgrades

Run the commands from the repository root. The release workflow packages these
same files, and CI runs the same root Compose stack.
