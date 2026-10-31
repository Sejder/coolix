"""Drive the coolix web UI in headless Chromium and save screenshots.

Run inside the VM test: screenshots.py OUTDIR SETUP_CODE CHROMIUM CHROMEDRIVER

It claims the fresh install with the setup code, adds a passkey (Chromium's
virtual authenticator), signs out and back in with it, deploys an app and
takes the README screenshots. The UI is used on http://localhost:8090:
passkeys need a secure context, which plain-HTTP coolix.test isn't.
"""
import sys
import time

from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.common.virtual_authenticator import VirtualAuthenticatorOptions
from selenium.webdriver.support import expected_conditions as ec
from selenium.webdriver.support.ui import WebDriverWait

out, code, chromium, chromedriver = sys.argv[1:5]
base = "http://localhost:8090"
password = "correct horse battery staple"

opts = Options()
opts.binary_location = chromium
for arg in ("--headless=new", "--no-sandbox", "--disable-gpu", "--force-device-scale-factor=1", "--hide-scrollbars"):
    opts.add_argument(arg)
d = webdriver.Chrome(service=Service(chromedriver), options=opts)
wait = WebDriverWait(d, 120)

authenticator = VirtualAuthenticatorOptions()
authenticator.protocol = VirtualAuthenticatorOptions.Protocol.CTAP2
authenticator.transport = VirtualAuthenticatorOptions.Transport.INTERNAL
authenticator.has_resident_key = True
authenticator.has_user_verification = True
authenticator.is_user_verified = True
d.add_virtual_authenticator(authenticator)


def shot(name, width=1400, height=900):
    d.set_window_size(width, height)
    time.sleep(0.5)
    d.save_screenshot(f"{out}/{name}.png")
    print("saved", name)


def text(selector):
    # Read through JS: elements are replaced as htmx and navigation swap the page.
    return d.execute_script(f"return document.querySelector('{selector}')?.textContent ?? ''") or ""


def flash():
    return text(".flash")


d.set_window_size(1400, 900)

# A fresh install sends everyone to the setup page. Screenshots use the
# light theme, and dark versions at the end.
d.get(base + "/")
d.execute_script("localStorage.setItem('coolix-theme', 'light')")
d.get(base + "/")
wait.until(ec.presence_of_element_located((By.NAME, "code")))
shot("setup", height=900)
d.find_element(By.NAME, "code").send_keys(code)
d.find_element(By.NAME, "password").send_keys(password)
d.find_element(By.NAME, "confirm").send_keys(password)
d.find_element(By.CSS_SELECTOR, "form[action='/setup'] button.primary").click()
wait.until(ec.text_to_be_present_in_element((By.CSS_SELECTOR, ".flash"), "Account created"))

# Add a passkey.
d.find_element(By.CSS_SELECTOR, "form[data-passkey-register] input[name=name]").send_keys("Test laptop")
d.find_element(By.CSS_SELECTOR, "form[data-passkey-register] button").click()
wait.until(lambda d: "Passkey added" in flash() or "not added" in text("[data-passkey-status]"))
assert "Passkey added" in flash(), text("[data-passkey-status]")
assert "Test laptop" in text("main")
shot("account", height=760)

# Sign out, then back in with the passkey.
d.find_element(By.CSS_SELECTOR, "form[action='/logout'] button").click()
wait.until(ec.presence_of_element_located((By.CSS_SELECTOR, "[data-passkey-login]")))
shot("login", height=760)
d.find_element(By.CSS_SELECTOR, "[data-passkey-login]").click()
wait.until(ec.presence_of_element_located((By.CSS_SELECTOR, "table.apps")))
print("signed in with a passkey")

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
wait.until(lambda d: "GET /healthz" in text("pre.log"))
shot("app", height=1500)

d.get(base + "/generations")
shot("generations", height=600)

# Dark theme, picked with the switch in the top bar.
d.get(base + "/")
while "dark" not in text("[data-theme-toggle]"):
    d.find_element(By.CSS_SELECTOR, "[data-theme-toggle]").click()
assert d.execute_script("return document.documentElement.dataset.theme") == "dark"
shot("dashboard-dark", height=760)
d.get(base + "/apps/hello")
wait.until(lambda d: "GET /healthz" in text("pre.log"))
shot("app-dark", height=1500)

d.quit()
