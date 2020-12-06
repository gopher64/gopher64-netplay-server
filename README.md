# m64p-netplay-server

## Dependencies

m64p-netplay-server requires qt@>=5.10

### Ubuntu

Qt 5.10 is most easily available on Ubuntu 20+. On older versions of Ubuntu, the `qt5-default` package will install an older version.

```sh
sudo apt install build-essential qt5-default libqt5websockets5-dev
```

### Fedora

For Fedora, replace the `qmake` command with `qmake-qt5`.

```sh
sudo dnf install git-all qt5-qtbase qt5-qtbase-devel qt5-qtwebsockets
```

## Building
```
git clone https://github.com/m64p/m64p-netplay-server.git
cd m64p-netplay-server
mkdir build
cd build
qmake ..
make -j4
```

## Running
```
cd m64p-netplay-server/build
./m64p-netplay-server --name "Server Name"
```

## Playing locally
The server is discoverable on a LAN (similar to how a Chromecast works). When the server is running, clients on the same LAN should find the server automatically.

## Port/firewall requirements
The server will be listening on ports 45000-45010, using TCP and UDP. Firewalls will need to be configured to allow connections on these ports.
