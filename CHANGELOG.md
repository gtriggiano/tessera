# Changelog

## 1.0.0 (2026-11-28)


### Features

* answer Envoy authorization checks with tessera serve ([2a85226](https://github.com/gtriggiano/tessera/commit/2a852263cb30830fccc2f887f96b62199ac15321))
* **config:** read the configuration file, with config validate and config print ([a9ffba7](https://github.com/gtriggiano/tessera/commit/a9ffba7575e590a92309c65bebc55b79d90905d6))
* decide requests with ordered rules written in CEL ([4f96cd2](https://github.com/gtriggiano/tessera/commit/4f96cd2b6236d979ecd58507e4380c79359055a5))
* enrich decisions with MMDB databases and declared upstream headers ([19c3319](https://github.com/gtriggiano/tessera/commit/19c331960c1e0fff9192addafa81182388c7fc60))
* match requests against sets of networks, AS numbers and strings ([b238a02](https://github.com/gtriggiano/tessera/commit/b238a02fa059ae1ae7fa55c8ed0afb6f523695ad))
* recognize the client software and match locations in geographic areas ([9676751](https://github.com/gtriggiano/tessera/commit/9676751f9e21b92df83fb0f6d37f411717fb2c44))


### Dependencies

* bump cel.dev/cel-go from 0.32.0 to 0.33.4 ([#5](https://github.com/gtriggiano/tessera/issues/5)) ([addb56a](https://github.com/gtriggiano/tessera/commit/addb56a086667d09472c56f0e4c98283858fbf16))
* bump distroless/static-debian13 from `25ac8f0` to `aafe993` ([#1](https://github.com/gtriggiano/tessera/issues/1)) ([8151c5d](https://github.com/gtriggiano/tessera/commit/8151c5df9961cfb5c73a649365748b80be843a5a))
