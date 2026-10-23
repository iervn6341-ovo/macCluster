import XCTest
@testable import MacCluster

final class ListeningScopeTests: XCTestCase {
    func testWildcardIsShownAsTheAddressItMeans() {
        XCTAssertEqual(PortRow.scope(of: ["*"]), "0.0.0.0（所有網路介面，區網可連）")
        XCTAssertEqual(PortRow.scope(of: ["::"]), "::（所有網路介面，區網可連）")
    }

    func testLoopbackOnlyIsThisMacOnly() {
        XCTAssertEqual(PortRow.scope(of: ["127.0.0.1"]), "127.0.0.1（僅此 Mac）")
        XCTAssertEqual(PortRow.scope(of: ["::1", "127.0.0.1"]), "::1（僅此 Mac）、127.0.0.1（僅此 Mac）")
    }

    func testASpecificInterfaceIsNamed() {
        XCTAssertEqual(PortRow.scope(of: ["192.0.2.5"]), "192.0.2.5（區網可連）")
    }

    func testRepeatedAddressesAreShownOnce() {
        XCTAssertEqual(PortRow.scope(of: ["*", "0.0.0.0"]), "0.0.0.0（所有網路介面，區網可連）")
    }
}
