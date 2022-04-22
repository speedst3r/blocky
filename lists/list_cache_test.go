package lists

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	. "github.com/0xERR0R/blocky/evt"

	. "github.com/0xERR0R/blocky/helpertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ListCache", func() {
	var (
		emptyFile, file1, file2, file3 *os.File
		server1, server2, server3      *httptest.Server
	)
	BeforeEach(func() {
		emptyFile = TempFile("#empty file\n\n")
		server1 = TestServer("blocked1.com\nblocked1a.com\n192.168.178.55")
		server2 = TestServer("blocked2.com")
		server3 = TestServer("blocked3.com\nblocked1a.com")

		file1 = TempFile("blocked1.com\nblocked1a.com")
		file2 = TempFile("blocked2.com")
		file3 = TempFile("blocked3.com\nblocked1a.com")

	})
	AfterEach(func() {
		_ = os.Remove(emptyFile.Name())
		_ = os.Remove(file1.Name())
		_ = os.Remove(file2.Name())
		_ = os.Remove(file3.Name())
		server1.Close()
		server2.Close()
		server3.Close()
	})

	Describe("List cache and matching", func() {
		When("Query with empty", func() {
			It("should not panic", func() {
				lists := map[string][]string{
					"gr0": {emptyFile.Name()},
				}
				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Second, &http.Transport{})

				found, group := sut.Match("", []string{"gr0"})
				Expect(found).Should(BeFalse())
				Expect(group).Should(BeEmpty())
			})
		})

		When("List is empty", func() {
			It("should not match anything", func() {
				lists := map[string][]string{
					"gr1": {emptyFile.Name()},
				}
				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Second, &http.Transport{})

				found, group := sut.Match("google.com", []string{"gr1"})
				Expect(found).Should(BeFalse())
				Expect(group).Should(BeEmpty())
			})
		})
		When("If timeout occurs", func() {
			var attempt uint64 = 1
			It("Should perform a retry", func() {
				failedDownloadCount := 0
				_ = Bus().SubscribeOnce(CachingFailedDownloadChanged, func(_ string) {
					failedDownloadCount++
				})

				// should produce a timeout on first attempt
				s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
					a := atomic.LoadUint64(&attempt)
					if a == 1 {
						time.Sleep(500 * time.Millisecond)
					} else {
						_, err := rw.Write([]byte("blocked1.com"))
						Expect(err).Should(Succeed())
					}
					atomic.AddUint64(&attempt, 1)
				}))
				defer s.Close()
				lists := map[string][]string{
					"gr1": {s.URL},
				}

				sut, _ := NewListCache(
					ListCacheTypeBlacklist, lists,
					0, 400*time.Millisecond, 3, time.Millisecond,
					&http.Transport{},
				)
				Eventually(func(g Gomega) {
					found, group := sut.Match("blocked1.com", []string{"gr1"})
					g.Expect(found).Should(BeTrue())
					g.Expect(group).Should(Equal("gr1"))
				}, "1s").Should(Succeed())

				Expect(failedDownloadCount).Should(Equal(1))
			})
		})
		When("a temporary err occurs on download", func() {
			var attempt uint64 = 1
			It("should not delete existing elements from group cache", func() {
				// should produce a timeout on second attempt
				s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
					a := atomic.LoadUint64(&attempt)
					if a != 1 {
						time.Sleep(200 * time.Millisecond)
					} else {
						_, err := rw.Write([]byte("blocked1.com"))
						Expect(err).Should(Succeed())
					}
					atomic.AddUint64(&attempt, 1)
				}))
				defer s.Close()
				lists := map[string][]string{
					"gr1": {s.URL, emptyFile.Name()},
				}

				sut, _ := NewListCache(
					ListCacheTypeBlacklist, lists,
					4*time.Hour, 100*time.Millisecond, 3, time.Millisecond,
					&http.Transport{},
				)
				By("Lists loaded without timeout", func() {
					Eventually(func(g Gomega) {
						found, group := sut.Match("blocked1.com", []string{"gr1"})
						g.Expect(found).Should(BeTrue())
						g.Expect(group).Should(Equal("gr1"))
					}, "1s").Should(Succeed())

				})

				sut.Refresh()

				By("List couldn't be loaded due to timeout", func() {
					found, group := sut.Match("blocked1.com", []string{"gr1"})
					Expect(found).Should(BeTrue())
					Expect(group).Should(Equal("gr1"))
				})
			})
		})
		When("err occurs on download", func() {
			var attempt uint64 = 1
			It("should delete existing elements from group cache", func() {
				// should produce a 404 err on second attempt
				s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
					a := atomic.LoadUint64(&attempt)
					if a != 1 {
						rw.WriteHeader(http.StatusNotFound)
					} else {
						_, err := rw.Write([]byte("blocked1.com"))
						Expect(err).Should(Succeed())
					}
					atomic.AddUint64(&attempt, 1)
				}))
				defer s.Close()
				lists := map[string][]string{
					"gr1": {s.URL},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Millisecond, &http.Transport{})
				By("Lists loaded without err", func() {
					Eventually(func(g Gomega) {
						found, group := sut.Match("blocked1.com", []string{"gr1"})
						g.Expect(found).Should(BeTrue())
						g.Expect(group).Should(Equal("gr1"))
					}, "1s").Should(Succeed())

				})

				sut.Refresh()

				By("List couldn't be loaded due to 404 err", func() {
					Eventually(func() bool {
						found, _ := sut.Match("blocked1.com", []string{"gr1"})
						return found
					}, "1s").Should(BeFalse())
				})
			})
		})
		When("Configuration has 3 external urls", func() {
			It("should download the list and match against", func() {
				lists := map[string][]string{
					"gr1": {server1.URL, server2.URL},
					"gr2": {server3.URL},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("blocked1.com", []string{"gr1", "gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("blocked1a.com", []string{"gr1", "gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("blocked1a.com", []string{"gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr2"))
			})
			It("should not match if no groups are passed", func() {
				lists := map[string][]string{
					"gr1":          {server1.URL, server2.URL},
					"gr2":          {server3.URL},
					"withDeadLink": {"http://wrong.host.name"},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("blocked1.com", []string{})
				Expect(found).Should(BeFalse())
				Expect(group).Should(BeEmpty())
			})
		})
		When("List will be updated", func() {
			It("event should be fired and contain count of elements in downloaded lists", func() {
				lists := map[string][]string{
					"gr1": {server1.URL},
				}

				resultCnt := 0

				_ = Bus().SubscribeOnce(BlockingCacheGroupChanged, func(listType ListCacheType, group string, cnt int) {
					resultCnt = cnt
				})

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 30*time.Second, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("blocked1.com", []string{})
				Expect(found).Should(BeFalse())
				Expect(group).Should(BeEmpty())
				Expect(resultCnt).Should(Equal(3))
			})
		})
		When("multiple groups are passed", func() {
			It("should match", func() {
				lists := map[string][]string{
					"gr1": {file1.Name(), file2.Name()},
					"gr2": {"file://" + file3.Name()},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 0, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("blocked1.com", []string{"gr1", "gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("blocked1a.com", []string{"gr1", "gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("blocked1a.com", []string{"gr2"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr2"))
			})
		})
		When("inline list content is defined", func() {
			It("should match", func() {
				lists := map[string][]string{
					"gr1": {"inlinedomain1.com\n#some comment\n#inlinedomain2.com"},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 0, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("inlinedomain1.com", []string{"gr1"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("inlinedomain1.com", []string{"gr1"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))
			})
		})
		When("inline regex content is defined", func() {
			It("should match", func() {
				lists := map[string][]string{
					"gr1": {"/^apple\\.(de|com)$/\n"},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 0, 3, time.Millisecond, &http.Transport{})

				found, group := sut.Match("apple.com", []string{"gr1"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))

				found, group = sut.Match("apple.de", []string{"gr1"})
				Expect(found).Should(BeTrue())
				Expect(group).Should(Equal("gr1"))
			})
		})
	})
	Describe("Configuration", func() {
		When("refresh is enabled", func() {
			It("should print list configuration", func() {
				lists := map[string][]string{
					"gr1": {server1.URL, server2.URL},
					"gr2": {"inline\ndefinition\n"},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, 0, 0, 3, time.Millisecond, &http.Transport{})

				c := sut.Configuration()
				Expect(c).Should(HaveLen(11))
			})
		})
		When("refresh is disabled", func() {
			It("should print 'refresh disabled'", func() {
				lists := map[string][]string{
					"gr1": {"file1", "file2"},
				}

				sut, _ := NewListCache(ListCacheTypeBlacklist, lists, -1, 0, 3, time.Millisecond, &http.Transport{})

				c := sut.Configuration()
				Expect(c).Should(ContainElement("refresh: disabled"))
			})
		})
	})
})
