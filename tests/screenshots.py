"""Drive the coolix web UI in headless Chromium and save screenshots.

Run inside the VM test: screenshots.py OUTDIR CHROMIUM CHROMEDRIVER
"""
import sys
import time

from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.support import expected_conditions as ec
from selenium.webdriver.support.ui import WebDriverWait

out, chromium, chromedriver = sys.argv[1:4]
base = "http://coolix.test"

opts = Options()
opts.binary_location = chromium
for arg in ("--headless=new", "--no-sandbox", "--disable-gpu", "--force-device-scale-factor=1", "--hide-scrollbars"):
    opts.add_argument(arg)
d = webdriver.Chrome(service=Service(chromedriver), options=opts)
wait = WebDriverWait(d, 120)


def shot(name, width=1400, height=900):
    d.set_window_size(width, height)
    time.sleep(0.5)
    d.save_screenshot(f"{out}/{name}.png")
    print("saved", name)


d.set_window_size(1400, 900)
d.get(base + "/login")
shot("login", height=700)
d.find_element(By.NAME, "password").send_keys("hunter2")
d.find_element(By.CSS_SELECTOR, "button.primary").click()
wait.until(ec.presence_of_element_located((By.CSS_SELECTOR, "table.apps")))

# Deploy hello from the UI and wait for the live log to finish.
d.get(base + "/apps/hello")
d.find_element(By.CSS_SELECTOR, ".pagehead button.primary").click()
wait.until(ec.text_to_be_present_in_element((By.CSS_SELECTOR, "[data-job-state]"), "succeeded"))
log = d.find_element(By.CSS_SELECTOR, "pre.log").text
assert "hello: stopping blue slot" in log, "activation output missing from the job log:\n" + log
shot("deploy", height=820)

d.get(base + "/")
shot("dashboard", height=760)

d.get(base + "/apps/hello")
# App logs stream in through coolix-query.
wait.until(lambda d: "GET /healthz" in d.find_element(By.CSS_SELECTOR, "pre.log").text)
shot("app", height=1500)

d.get(base + "/generations")
shot("generations", height=600)

d.quit()
